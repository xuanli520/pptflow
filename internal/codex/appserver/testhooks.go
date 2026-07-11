package appserver

import (
	"context"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/executor"
)

type TestSessionProbe struct {
	session *appServerCodexReviewSession
}

func NewSessionProbeForTest(logPath, turnID string, maxOutputBytes int, onDelta func(Update)) *TestSessionProbe {
	return newSessionProbeForTest(Request{
		LogPath:        logPath,
		MaxOutputBytes: maxOutputBytes,
		OnDelta:        onDelta,
	}, turnID, nil)
}

func NewSessionProbeWithProcessContextForTest(logPath string, processCtx context.Context) *TestSessionProbe {
	return newSessionProbeForTest(Request{LogPath: logPath}, "", processCtx)
}

func FormatAggregatedDeltaLogLineForTest(turnID, itemID, text string) string {
	return formatAggregatedDeltaLogLine(aggregatedDeltaLog{turnID: turnID, itemID: itemID, text: text})
}

func newSessionProbeForTest(req Request, turnID string, processCtx context.Context) *TestSessionProbe {
	return &TestSessionProbe{session: &appServerCodexReviewSession{
		req:                   req,
		processCtx:            processCtx,
		done:                  make(chan struct{}),
		responses:             map[int]chan appServerRPCMessage{},
		turnID:                turnID,
		items:                 map[string]string{},
		deltas:                map[string]string{},
		deltaLogged:           map[string]bool{},
		deltaPreview:          map[string]string{},
		deltaPreviewTruncated: map[string]bool{},
		itemDone:              map[string]bool{},
	}}
}

func (p *TestSessionProbe) Complete(command string, err error) {
	if p == nil || p.session == nil {
		return
	}
	p.session.complete(executor.Result{Command: command, Err: err}, err)
}

func (p *TestSessionProbe) CompleteStreamError(stream string, err error) {
	if p == nil || p.session == nil {
		return
	}
	p.session.completeStreamError(stream, err)
}

func (p *TestSessionProbe) ReadStdout(stream string) {
	if p == nil || p.session == nil {
		return
	}
	p.session.readStdout(strings.NewReader(stream))
}

func (p *TestSessionProbe) RecordDelta(turnID, itemID, delta string) {
	if p == nil || p.session == nil {
		return
	}
	p.session.recordDelta(turnID, itemID, delta)
}

func (p *TestSessionProbe) RecordCompletedItem(turnID, itemID, text string) {
	if p == nil || p.session == nil {
		return
	}
	p.session.recordCompletedItem(turnID, itemID, text)
}

func (p *TestSessionProbe) ResultStdout() string {
	if p == nil || p.session == nil {
		return ""
	}
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	return p.session.result.Result.Stdout
}

func (p *TestSessionProbe) Err() error {
	if p == nil || p.session == nil {
		return nil
	}
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	return p.session.err
}

func (p *TestSessionProbe) Completed() bool {
	if p == nil || p.session == nil {
		return false
	}
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	return p.session.completed
}

func (p *TestSessionProbe) DoneClosed() bool {
	if p == nil || p.session == nil || p.session.done == nil {
		return false
	}
	select {
	case <-p.session.done:
		return true
	default:
		return false
	}
}

func (p *TestSessionProbe) FinalReport() string {
	if p == nil || p.session == nil {
		return ""
	}
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	return p.session.finalReportLocked()
}

func (p *TestSessionProbe) DeltaForItem(itemID string) string {
	if p == nil || p.session == nil {
		return ""
	}
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	return p.session.deltas[itemID]
}

func (p *TestSessionProbe) ItemOrderLen() int {
	if p == nil || p.session == nil {
		return 0
	}
	p.session.mu.Lock()
	defer p.session.mu.Unlock()
	return len(p.session.itemOrder)
}
