package restgw_test

// lookup turns a static map into the env-lookup callback shape
// every restgw config helper reads (so tests don't have to
// touch the real os.Getenv environment).
func lookup(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}
