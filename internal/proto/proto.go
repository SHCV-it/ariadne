// Package proto defines the socket protocol.
//
// Framing is a 4-byte big-endian length followed by JSON. JSON was chosen over
// msgpack because payloads are ~200 bytes and encode/decode measures in single
// microseconds — three orders of magnitude under the 10ms budget. Adding a
// serialization dependency to save 2µs would be optimizing the wrong thing,
// and a human-readable protocol makes `socat` debugging trivial.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"ariadne/internal/core"
)

const (
	OpQuery   = "query"
	OpIngest  = "ingest"
	OpAccept  = "accept"
	OpReject  = "reject"
	OpStats   = "stats"
	OpHarvest = "harvest"
	OpTrain   = "train"
	OpForget  = "forget"
	OpImport  = "import"
	OpPing    = "ping"
)

const MaxFrame = 8 << 20

type Request struct {
	Op string `json:"op"`

	// Query
	Buffer    string `json:"buf,omitempty"`
	Cursor    int    `json:"cur,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	GitRoot   string `json:"repo,omitempty"`
	GitBranch string `json:"branch,omitempty"`
	Host      string `json:"host,omitempty"`
	Session   string `json:"sess,omitempty"`
	Limit     int    `json:"n,omitempty"`

	// Ingest
	Event  *core.Event   `json:"ev,omitempty"`
	Events []*core.Event `json:"evs,omitempty"` // batch, for OpImport

	// Accept / Reject
	Chosen int    `json:"chosen,omitempty"`
	Text   string `json:"text,omitempty"`

	// Forget / Import
	Pattern string `json:"pattern,omitempty"`
	Path    string `json:"path,omitempty"`
	Format  string `json:"format,omitempty"`
}

type Candidate struct {
	Text    string `json:"t"`
	Display string `json:"d,omitempty"`
	Desc    string `json:"c,omitempty"`
	Count   int32  `json:"n,omitempty"`
	Context string `json:"x,omitempty"`
	Source  string `json:"s,omitempty"`
	Score   int    `json:"p,omitempty"` // 0..100, for display only
}

type Response struct {
	OK   bool   `json:"ok"`
	Err  string `json:"err,omitempty"`

	// Query
	Ghost      string      `json:"ghost,omitempty"`
	Candidates []Candidate `json:"cands,omitempty"`
	OwnsToken  bool        `json:"owns"`
	ElapsedUS  int64       `json:"us,omitempty"`

	// Stats / generic
	Info map[string]any `json:"info,omitempty"`
	Text string         `json:"text,omitempty"`
}

func Write(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return errors.New("frame too large")
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func Read(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return errors.New("frame too large")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}
