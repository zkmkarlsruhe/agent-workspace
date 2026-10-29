// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// lease binds every manager HTTP call and proxied request to one container
// generation. The slot address is reused after an account deletion, so
// DeleteAccount ends the lease: calls in flight are cancelled and waited
// for, later ones fail before they leave, and no request carrying this
// generation's password reaches the next tenant. A nil lease binds nothing.
type lease struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	ended  bool
	wg     sync.WaitGroup
}

func newLease() *lease {
	ctx, cancel := context.WithCancel(context.Background())
	return &lease{ctx: ctx, cancel: cancel}
}

// bind returns ctx cancelled with the lease and the release of the hold;
// false once the lease ended.
func (l *lease) bind(ctx context.Context) (context.Context, func(), bool) {
	if l == nil {
		return ctx, func() {}, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return nil, nil, false
	}
	l.wg.Add(1)
	ctx, stop := context.WithCancel(ctx)
	unhook := context.AfterFunc(l.ctx, stop)
	return ctx, sync.OnceFunc(func() { unhook(); stop(); l.wg.Done() }), true
}

// stop cancels the generation's calls in flight and refuses new ones.
func (l *lease) stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.ended = true
	l.mu.Unlock()
	l.cancel()
}

// end is stop, then waits until every call returned.
func (l *lease) end() {
	if l == nil {
		return
	}
	l.stop()
	l.wg.Wait()
}

type leaseTransport struct {
	l    *lease
	base http.RoundTripper
}

func (t leaseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, release, ok := t.l.bind(req.Context())
	if !ok {
		return nil, ErrNotRunning
	}
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = leaseBody{resp.Body, release}
	return resp, nil
}

type leaseBody struct {
	io.ReadCloser
	release func()
}

func (b leaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

// Bind holds the target's generation for one proxied request: ctx is
// cancelled when the generation ends, release must run when the request is
// done; false = the generation ended, do not send.
func (t Target) Bind(ctx context.Context) (context.Context, func(), bool) { return t.lease.bind(ctx) }
