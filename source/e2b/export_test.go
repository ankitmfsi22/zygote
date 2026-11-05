package e2b

// Resolve exposes resolve for tests.
func (a *Adapter) Resolve(rel string) (string, error) { return a.resolve(rel) }
