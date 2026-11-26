package bt

import (
	"context"
	"slices"
	"sync"
)

const (
	maxScopeBreadcrumbs = 32
	maxScopeAttachments = 32
)

// Scope holds attributes, breadcrumbs and file attachments for one unit of
// work, such as a request. Create one with WithScope and get it back with
// ScopeFromContext.
//
// Reports made with a context that carries a scope, such as
// Client.ReportContext, include the data of that scope and of its parent
// scopes. Reports keep the newest Config.MaxBreadcrumbs breadcrumbs of the
// client and the scopes together.
//
// A Scope is safe for concurrent use. Methods on a nil *Scope do nothing.
type Scope struct {
	parent *Scope

	mu          sync.Mutex
	attributes  map[string]any
	crumbs      *breadcrumbRing
	attachments []string
}

type scopeKey struct{}

// WithScope returns a copy of ctx that carries a new, empty Scope. When ctx
// already carries a scope, the new one is its child, and the child's
// attributes win. A nil ctx is treated as context.Background().
func WithScope(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, scopeKey{}, &Scope{parent: ScopeFromContext(ctx)})
}

// ScopeFromContext returns the innermost Scope that ctx carries, or nil when
// it carries none or ctx is nil.
func ScopeFromContext(ctx context.Context) *Scope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(scopeKey{}).(*Scope)
	return s
}

// SetAttribute sets an attribute on reports captured under the scope or its
// child scopes. A nil value removes the attribute from those reports, SDK
// defaults included. The value is copied and converted before the call
// returns, as described on ReportData.
func (s *Scope) SetAttribute(key string, value any) {
	if s == nil {
		return
	}
	frozen := freezeValue(value, legacyValueLimits())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attributes == nil {
		s.attributes = map[string]any{}
	}
	s.attributes[key] = frozen
}

// SetAttributes sets several attributes as SetAttribute does.
func (s *Scope) SetAttributes(attrs map[string]any) {
	if s == nil {
		return
	}
	frozen := freezeAttributes(attrs, legacyValueLimits())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attributes == nil {
		s.attributes = make(map[string]any, len(frozen))
	}
	for k, v := range frozen {
		s.attributes[k] = v
	}
}

// AddBreadcrumb records a breadcrumb on reports captured under the scope or
// its child scopes. Its attributes are copied before the call returns. Each
// scope keeps its newest 32 breadcrumbs.
func (s *Scope) AddBreadcrumb(b Breadcrumb) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.crumbs == nil {
		s.crumbs = newBreadcrumbRing(maxScopeBreadcrumbs)
	}
	r := s.crumbs
	s.mu.Unlock()
	r.add(b)
}

// AddAttachment uploads the file at path with reports captured under the
// scope or its child scopes. The file comes after the Config.AttachmentPaths
// files, under the same limits. The file is read when the report is sent and
// must stay in place until then, for example until Client.Flush returns true.
// A file missing at send time is skipped. Empty and repeated paths are
// ignored. Each scope keeps its first 32 paths.
func (s *Scope) AddAttachment(path string) {
	if s == nil || path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.attachments) < maxScopeAttachments && !slices.Contains(s.attachments, path) {
		s.attachments = append(s.attachments, path)
	}
}

func scopeChain(ctx context.Context) []*Scope {
	var chain []*Scope
	for s := ScopeFromContext(ctx); s != nil; s = s.parent {
		chain = append(chain, s)
	}
	slices.Reverse(chain)
	return chain
}

func (s *Scope) copyAttributes(dst map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.attributes {
		dst[k] = v
	}
}

func (s *Scope) breadcrumbs() []Breadcrumb {
	s.mu.Lock()
	r := s.crumbs
	s.mu.Unlock()
	return r.snapshot()
}

func scopeAttachments(chain []*Scope) []string {
	var paths []string
	for _, s := range chain {
		s.mu.Lock()
		paths = append(paths, s.attachments...)
		s.mu.Unlock()
	}
	return paths
}
