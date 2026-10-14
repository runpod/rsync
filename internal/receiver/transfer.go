package receiver

import (
	"os"

	"github.com/runpod/rsync/internal/log"
	"github.com/runpod/rsync/internal/progress"
	"github.com/runpod/rsync/internal/rsyncopts"
	"github.com/runpod/rsync/internal/rsyncos"
	"github.com/runpod/rsync/internal/rsyncwire"
)

// TransferOpts is a subset of Opts which is required for implementing a receiver.
type TransferOpts struct {
	Verbose  bool
	DryRun   bool
	Server   bool
	Progress bool

	DeleteMode        bool
	PreserveGid       bool
	PreserveUid       bool
	PreserveLinks     bool
	PreservePerms     bool
	PreserveDevices   bool
	PreserveSpecials  bool
	PreserveTimes     bool
	PreserveHardlinks bool
	IgnoreTimes       bool
	AlwaysChecksum    bool
	DoFsync           bool

	InfoGTE  func(rsyncopts.InfoLevel, uint16) bool
	DebugGTE func(rsyncopts.DebugLevel, uint16) bool

	KeepPartial bool
}

type Transfer struct {
	// config
	Logger   log.Logger
	Opts     *TransferOpts
	Dest     string
	DestRoot *os.Root
	Env      *rsyncos.Env
	Progress progress.Printer

	// state
	Conn     *rsyncwire.Conn
	Seed     int32
	IOErrors int32
	// A sender that cannot read a file skips it, and before protocol 30 says
	// nothing about having done so, so the gap between these is the only sign.
	// Written by GenerateFiles and RecvFiles, so read only once both finish.
	Requested       int32
	Received        int32
	Users           map[int32]mapping
	Groups          map[int32]mapping
	retouchDirPerms bool
}

// MissingFiles reports how many files the sender was asked for but never sent.
// Only meaningful once the transfer has finished.
func (rt *Transfer) MissingFiles() int32 {
	if rt.Received >= rt.Requested {
		return 0
	}
	return rt.Requested - rt.Received
}

func (rt *Transfer) listOnly() bool { return rt.Dest == "" }
