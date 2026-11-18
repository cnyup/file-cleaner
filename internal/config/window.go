package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseWindow 解析 "HH:MM-HH:MM" 时间窗，返回起止分钟数（当日 0 点起）。
// 支持跨天窗口（start > end，如 "23:00-06:00" 表示 23:00 到次日 06:00）。
// 语义约定：起点含、终点不含（07:00 窗口在 07:00:00 整即视为窗外——
// 边界处宁可少跑一分钟，不多跑一秒）。
func ParseWindow(w string) (start, end int, err error) {
	if w == "" {
		return 0, 0, fmt.Errorf("empty window")
	}
	parts := strings.SplitN(w, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("window %q: want HH:MM-HH:MM", w)
	}
	s, err := parseHHMM(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("window %q: %w", w, err)
	}
	e, err := parseHHMM(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("window %q: %w", w, err)
	}
	return s, e, nil
}

func parseHHMM(s string) (int, error) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("time %q: want HH:MM", s)
	}
	if len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, fmt.Errorf("time %q: HH and MM must be two digits", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("time %q: %w", s, err)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("time %q: %w", s, err)
	}
	if h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, fmt.Errorf("time %q: out of range", s)
	}
	return h*60 + m, nil
}

// InWindow 判定时刻 at 是否在窗口内（本地时区）。起点含、终点不含；
// 跨天窗口（start > end）自动处理：at >= start || at < end。
func InWindow(w string, at time.Time) (bool, error) {
	s, e, err := ParseWindow(w)
	if err != nil {
		return false, err
	}
	cur := at.Hour()*60 + at.Minute()
	if s == e {
		// 起止相同按全天然窗口处理（00:00-24:00 的简写形态）。
		return true, nil
	}
	if s < e {
		return cur >= s && cur < e, nil
	}
	// 跨天：[s, 24:00) ∪ [00:00, e)
	return cur >= s || cur < e, nil
}
