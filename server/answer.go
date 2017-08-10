package server

import (
	"context"
	"errors"

	"zombiezen.com/go/capnproto2"
	"zombiezen.com/go/capnproto2/internal/chanmu"
)

// answerQueue is a queue of method calls to make after an earlier method call
// finishes.  The queue is a fixed size, and will return overloaded errors once
// the queue is full.
type answerQueue struct {
	mu chanmu.Mutex
	q  []qent

	// answers is filled in after drain().
	// The first element is always the answer passed to drain(),
	// the remaining elements are answers for each method call in q.
	answers []*capnp.Answer
}

// qent is a single entry in an answerQueue.
type qent struct {
	ctx   context.Context
	basis int
	path  clientPath
	capnp.Recv

	p       *capnp.Promise // resolved during drain
	settled chan struct{}  // closed after finish is set
	finish  capnp.ReleaseFunc
}

// newAnswerQueue creates a new answer queue of the given size.
func newAnswerQueue(n int) *answerQueue {
	return &answerQueue{
		mu: chanmu.New(),
		q:  make([]qent, 0, n),
	}
}

// drain ties the outcome of p to dst.
//
// TODO(maybe): split this into fulfill/reject instead.
func (aq *answerQueue) drain(dst *capnp.Answer, p *capnp.Promise) {
	if dst == nil {
		panic("answerQueue.drain called with nil Answer")
	}
	aq.mu.Lock()
	if len(aq.answers) > 0 {
		panic("double call of answerQueue.drain")
	}
	aq.answers = make([]*capnp.Answer, len(aq.q)+1)
	aq.answers[0] = dst
	for i := range aq.q {
		ent := &aq.q[i]
		target := aq.answers[ent.basis]
		// TODO(soon): release lock while calling RecvCall and allow concurrent calls to add to queue.
		aq.answers[1+i], ent.finish = target.RecvCall(ent.ctx, ent.path.transform(), ent.Recv)

		// Drop references to arguments as soon as possible.
		ent.ctx = nil
		ent.path = ""
		ent.Recv = capnp.Recv{}
	}
	// Make results observable to application code.
	for i := range aq.q {
		ent := &aq.q[i]
		ent.p.Join(aq.answers[1+i])
		ent.p = nil
		close(ent.settled)
	}
	aq.q = nil
	p.Join(dst)
	aq.mu.Unlock()
}

func (aq *answerQueue) PipelineRecv(ctx context.Context, transform []capnp.PipelineOp, r capnp.Recv) (*capnp.Answer, capnp.ReleaseFunc) {
	return queueCaller{aq, 0}.PipelineRecv(ctx, transform, r)
}

func (aq *answerQueue) PipelineSend(ctx context.Context, transform []capnp.PipelineOp, r capnp.Send) (*capnp.Answer, capnp.ReleaseFunc) {
	return queueCaller{aq, 0}.PipelineSend(ctx, transform, r)
}

// queueCaller is a client that enqueues calls to an answerQueue.
type queueCaller struct {
	aq    *answerQueue
	basis int
}

func (qc queueCaller) PipelineRecv(ctx context.Context, transform []capnp.PipelineOp, r capnp.Recv) (*capnp.Answer, capnp.ReleaseFunc) {
	if err := qc.aq.mu.TryLock(ctx); err != nil {
		r.ReleaseArgs()
		return capnp.ErrorAnswer(err), func() {}
	}
	if len(qc.aq.answers) > 0 {
		// drain finished after PipelineRecv started but before obtaining lock.
		a := qc.aq.answers[qc.basis]
		qc.aq.mu.Unlock()
		return a.RecvCall(ctx, transform, r)
	}
	nq := len(qc.aq.q)
	if nq >= cap(qc.aq.q) {
		qc.aq.mu.Unlock()
		// TODO(someday): classify as overloaded
		r.ReleaseArgs()
		return capnp.ErrorAnswer(errors.New("capnp server: too many calls on pipelined answer")), func() {}
	}
	qc.aq.q = append(qc.aq.q, qent{
		ctx:   ctx,
		basis: qc.basis,
		path:  clientPathFromTransform(transform),
		Recv:  r,
		p: capnp.NewPromise(queueCaller{
			aq:    qc.aq,
			basis: 1 + nq,
		}),
		settled: make(chan struct{}),
	})
	ent := &qc.aq.q[nq]
	qc.aq.mu.Unlock()
	return ent.p.Answer(), func() {
		<-ent.settled
		ent.finish()
	}
}

func (qc queueCaller) PipelineSend(ctx context.Context, transform []capnp.PipelineOp, s capnp.Send) (*capnp.Answer, capnp.ReleaseFunc) {
	if s.PlaceArgs == nil {
		return qc.PipelineRecv(ctx, transform, capnp.Recv{
			Method:      s.Method,
			Options:     s.Options,
			ReleaseArgs: func() {},
		})
	}
	args, err := newBlankStruct(s.ArgsSize)
	if err != nil {
		return capnp.ErrorAnswer(err), func() {}
	}
	if err := s.PlaceArgs(args); err != nil {
		return capnp.ErrorAnswer(err), func() {}
	}
	return qc.PipelineRecv(ctx, transform, capnp.Recv{
		Args:    args,
		Method:  s.Method,
		Options: s.Options,
		ReleaseArgs: func() {
			// TODO(someday): log error from ClearCaps
			args.Message().Reset(nil)
		},
	})
}

// clientPath is an encoded version of a list of pipeline operations.
// It is suitable as a map key.
//
// It specifically ignores default values, because a capability can't have a
// default value other than null.
type clientPath string

func clientPathFromTransform(ops []capnp.PipelineOp) clientPath {
	buf := make([]byte, 0, len(ops)*2)
	for i := range ops {
		f := ops[i].Field
		buf = append(buf, byte(f&0x00ff), byte(f&0xff00))
	}
	return clientPath(buf)
}

func (cp clientPath) transform() []capnp.PipelineOp {
	ops := make([]capnp.PipelineOp, len(cp)/2)
	for i := range ops {
		ops[i].Field = uint16(cp[i*2])<<8 | uint16(cp[i*2+1])
	}
	return ops
}
