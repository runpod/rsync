package sender

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/runpod/rsync/internal/rsyncwire"
)

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// The receiver picks the index it asks for, so one outside the file list must
// end the transfer, not index past the list or be echoed back.
func TestSendFilesRejectsIndexOutsideFileList(t *testing.T) {
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
			var in, out bytes.Buffer
			binary.Write(&in, binary.LittleEndian, tt.idx)
			st := &Transfer{Conn: &rsyncwire.Conn{Reader: io.NopCloser(&in), Writer: nopWriteCloser{&out}}}

			err := st.SendFiles(&fileList{Files: []file{{}}})
			if err == nil || !strings.Contains(err.Error(), "outside the 1-entry file list") {
				t.Fatalf("SendFiles = %v, want an out-of-range error", err)
			}
			if out.Len() != 0 {
				t.Errorf("the index was echoed back: % x", out.Bytes())
			}
		})
	}
}
