package proc

// statFSType reports no tmpfs on darwin (it has none; dev builds only): secret files go to
// <config>/run with the warning.
func statFSType(string) (int64, error) { return 0, nil }
