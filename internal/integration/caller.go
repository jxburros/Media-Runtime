package integration

import goruntime "runtime"

func callerFile() (uintptr, string, int, bool) { return goruntime.Caller(0) }
