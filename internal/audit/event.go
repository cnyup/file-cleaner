package audit

import "time"

func nowUnixNano() int64 { return time.Now().UnixNano() }
