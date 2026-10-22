package store

import "time"

// The database stores timestamps as integers where 0 means "not set". These
// helpers map 0 to and from the zero time.Time.

func toUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func toUnixMicro(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMicro()
}

func fromUnix(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

func fromUnixMicro(usec int64) time.Time {
	if usec == 0 {
		return time.Time{}
	}
	return time.UnixMicro(usec)
}
