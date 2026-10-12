package postgres

import "context"

// SearchPathForTest reads the applier session's search_path, so a test can
// prove an operation left the session's settings as it found them.
func SearchPathForTest(ctx context.Context, a *Applier) (string, error) {
	var sp string
	err := a.conn.QueryRow(ctx, "SHOW search_path").Scan(&sp)
	return sp, err
}
