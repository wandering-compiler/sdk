package postgres

import "context"

// SearchPathForTest reads the applier session's search_path, so a test can
// prove an operation left the session's settings as it found them.
func SearchPathForTest(ctx context.Context, a *Applier) (string, error) {
	var sp string
	err := a.conn.QueryRow(ctx, "SHOW search_path").Scan(&sp)
	return sp, err
}

// SetCheckLimitsForTest replaces the temp_file_limit and work_mem a check
// runs under, until the returned func restores them.
func SetCheckLimitsForTest(tempFileLimit, workMem string) func() {
	t, w := checkTempFileLimit, checkWorkMem
	checkTempFileLimit, checkWorkMem = tempFileLimit, workMem
	return func() { checkTempFileLimit, checkWorkMem = t, w }
}
