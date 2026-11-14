package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// maxEventLine is the longest line of a stream the gateway will hold in
// memory to read. A longer line still reaches the caller. The events that
// carry token counts are far shorter.
const maxEventLine = 1 << 20 // 1 MB

// stream passes a streamed answer to the caller piece by piece as it arrives,
// and reads the model and token counts out of it on the way through.
//
// If the caller goes away, or stops taking pieces, the gateway stops sending
// but keeps reading, because the final token counts come at the end of the
// stream. giveUp cancels the upstream call if that takes longer than drainLimit.
func (g *gateway) stream(w http.ResponseWriter, resp *http.Response, name string, giveUp func()) {
	startAnswer(w, resp)
	// Go holds written bytes back until it has a few thousand of them. Flush
	// sends what there is, so the caller sees each piece right away.
	flusher := http.NewResponseController(w)
	flusher.Flush()

	var counter streamCounter
	callerGone := false
	piece := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(piece)
		if n > 0 {
			counter.read(piece[:n])
		}
		if n > 0 && !callerGone {
			// A caller that is connected but has stopped reading would
			// otherwise block this write, and the counting, for good.
			flusher.SetWriteDeadline(time.Now().Add(g.writeTimeout))
			if _, err := w.Write(piece[:n]); err != nil || flusher.Flush() != nil {
				callerGone = true
				timer := time.AfterFunc(g.drainLimit, giveUp)
				defer timer.Stop()
			}
		}
		if err != nil {
			break // the stream ended, or the gateway gave up on it
		}
	}

	model, u, ok := counter.result()
	if !ok {
		log.Printf("caller=%s stream had nothing to count", name)
		return
	}
	g.record(name, model, u)
}

// streamCounter follows a Messages API event stream. The stream is lines of
// text, and each event has a line that starts "data:" followed by JSON:
//
//	message_start        names the model and gives the first token counts
//	content_block_delta  carries a piece of the answer
//	message_delta        gives the token counts so far, replacing earlier ones
//	message_stop         ends the answer
type streamCounter struct {
	line     []byte // the line being read, which may arrive split across pieces
	tooLong  bool   // the current line passed maxEventLine and is being skipped
	model    string
	usage    usage
	started  bool  // message_start was seen
	finished bool  // message_stop was seen
	answered int64 // bytes of answer text seen in content_block_delta events
}

// read takes the next piece of the stream, which can end anywhere, even in
// the middle of a line.
func (c *streamCounter) read(piece []byte) {
	for len(piece) > 0 {
		end := bytes.IndexByte(piece, '\n')
		if end < 0 {
			c.keep(piece)
			return
		}
		c.keep(piece[:end])
		if !c.tooLong {
			c.event(c.line)
		}
		c.line, c.tooLong = c.line[:0], false
		piece = piece[end+1:]
	}
}

func (c *streamCounter) keep(part []byte) {
	if c.tooLong || len(c.line)+len(part) > maxEventLine {
		c.line, c.tooLong = c.line[:0], true
		return
	}
	c.line = append(c.line, part...)
}

func (c *streamCounter) event(line []byte) {
	data, isData := bytes.CutPrefix(line, []byte("data:"))
	if !isData {
		return
	}
	var event struct {
		Type    string `json:"type"`
		Message struct {
			Model string          `json:"model"`
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		Usage json.RawMessage `json:"usage"`
		Delta struct {
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"` // stands in for thinking that is not shown
		} `json:"delta"`
	}
	if json.Unmarshal(data, &event) != nil {
		return // not an event this code knows; new kinds can be added to the API
	}
	switch event.Type {
	case "message_start":
		c.started, c.model = true, event.Message.Model
		c.merge(event.Message.Usage)
	case "message_delta":
		c.merge(event.Usage)
	case "content_block_delta":
		c.answered += int64(len(event.Delta.Text) + len(event.Delta.PartialJSON) +
			len(event.Delta.Thinking) + len(event.Delta.Signature))
	case "message_stop":
		c.finished = true
	}
}

// merge lays newer token counts over the ones already held. The counts in a
// stream are running totals, so a count that is present replaces the old
// one, and a count that is left out keeps its last value. Unmarshal into a
// filled struct does exactly that.
func (c *streamCounter) merge(counts json.RawMessage) {
	if len(counts) == 0 {
		return
	}
	split := c.usage.CacheCreation
	json.Unmarshal(counts, &c.usage)
	if c.usage.CacheCreation == nil {
		c.usage.CacheCreation = split // a null does not erase the cache split
	}
}

// result is the model and token counts to charge for. ok is false when the
// stream never got as far as naming a model.
//
// The final output count comes at the end of a stream. A stream that was cut
// short never sends it, though the tokens were produced. In that case the
// output is estimated from the answer that did arrive, at 3 bytes a token,
// unless the stream reported more. This is a last resort: the gateway reads a
// stream to its end even after the caller has gone, so it is only needed when
// the upstream breaks off or the drain limit runs out.
func (c *streamCounter) result() (model string, u usage, ok bool) {
	if !c.started {
		return "", usage{}, false
	}
	u = c.usage
	if !c.finished {
		u.OutputTokens = max(u.OutputTokens, (c.answered+2)/3)
	}
	return c.model, u, true
}
