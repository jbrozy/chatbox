package terminal

import "context"

// Unavailable ersetzt das Backend, wenn kein Kubernetes erreichbar ist. Policies lassen
// sich dann weiter verwalten, alle Terminal-Zugriffe schlagen mit Err fehl.
type Unavailable struct {
	Err error
	Cfg Config
}

func (u Unavailable) Ensure(context.Context, Key, Spec) (Instance, error) { return Instance{}, u.Err }
func (u Unavailable) Invalidate(Key)                                      {}
func (u Unavailable) Touch(Key)                                           {}
func (u Unavailable) Connected(Key) func()                                { return func() {} }
func (u Unavailable) ActiveConnections() int                              { return 0 }
func (u Unavailable) List(context.Context) ([]Info, error)                { return nil, u.Err }
func (u Unavailable) Defaults() Config                                    { return u.Cfg }

func (u Unavailable) Refresh(context.Context, Filter, bool, bool) (RefreshResult, error) {
	return RefreshResult{}, u.Err
}
