package receiver

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/runpod/rsync/internal/log"
	"github.com/runpod/rsync/internal/rsyncwire"
)

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// wireConn is a connection whose peer sends the given int32s and then hangs up.
func wireConn(ints ...int32) *rsyncwire.Conn {
	var buf bytes.Buffer
	for _, i := range ints {
		binary.Write(&buf, binary.LittleEndian, i)
	}
	return &rsyncwire.Conn{Reader: io.NopCloser(&buf), Writer: nopWriteCloser{io.Discard}}
}

// The sender picks the index it answers for, so one outside the file list must
// end the transfer, not index past the list.
func TestRecvFilesRejectsIndexOutsideFileList(t *testing.T) {
	tests := []struct {
		name string
		idx  int32
	}{
		{name: "one past the end", idx: 1},
		{name: "far past the end", idx: 1 << 30},
		{name: "negative, but not the end marker", idx: -2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &Transfer{Logger: log.New(io.Discard), Opts: &TransferOpts{DryRun: true}, Conn: wireConn(tt.idx)}
			err := rt.RecvFiles([]*File{{Name: "f"}})
			if err == nil || !strings.Contains(err.Error(), "outside the 1-entry file list") {
				t.Fatalf("RecvFiles = %v, want an out-of-range error", err)
			}
		})
	}
}

func TestRecoveredTurnsAPanicIntoAnError(t *testing.T) {
	tests := []struct {
		name    string
		f       func() error
		wantErr string
	}{
		{name: "no panic, no error", f: func() error { return nil }},
		{name: "an error passes through", f: func() error { return io.ErrUnexpectedEOF }, wantErr: "unexpected EOF"},
		{name: "a panic becomes an error", f: func() error { panic("boom") }, wantErr: "panic in test: boom"},
		{name: "a runtime panic becomes an error", f: func() error { var l []*File; _ = l[3]; return nil }, wantErr: "index out of range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := recovered("test", tt.f)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("recovered = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("recovered = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// A panic on either of Do's goroutines must come back from Do as an error.
func TestDoReturnsAPanicInTheTransferAsAnError(t *testing.T) {
	// Opts without its DebugGTE func panics in the generator and the receiver.
	rt := &Transfer{Logger: log.New(io.Discard), Opts: &TransferOpts{}, Conn: wireConn(0)}
	_, err := rt.Do(rt.Conn, []*File{{Name: "f"}}, true)
	if err == nil || !strings.Contains(err.Error(), "panic in ") {
		t.Fatalf("Do = %v, want a recovered panic", err)
	}
}
