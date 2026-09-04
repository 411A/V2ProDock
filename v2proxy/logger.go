package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	clog "github.com/charmbracelet/log"
)

const (
	levelInfo = iota
	levelDebug
)

var currentLogLevel = levelInfo
var useColor = true
var logger *clog.Logger

func init() {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug", "trace", "verbose", "all":
		currentLogLevel = levelDebug
	default:
		currentLogLevel = levelInfo
	}
	if os.Getenv("NO_COLOR") != "" {
		useColor = false
	} else if v := os.Getenv("LOG_COLOR"); strings.EqualFold(v, "0") || strings.EqualFold(v, "false") || strings.EqualFold(v, "off") {
		useColor = false
	}
	lvl := clog.InfoLevel
	if currentLogLevel >= levelDebug {
		lvl = clog.DebugLevel
	}
	opts := clog.Options{
		Level:           lvl,
		TimeFormat:      logTimeFormat,
		ReportCaller:    currentLogLevel >= levelDebug,
		CallerOffset:    1,
		ReportTimestamp: true,
		Prefix:          "",
	}
	logger = clog.NewWithOptions(os.Stderr, opts)
	if !useColor {
		s := clog.DefaultStyles()
		s.Timestamp = lipgloss.NewStyle()
		s.Caller = lipgloss.NewStyle()
		s.Prefix = lipgloss.NewStyle()
		s.Message = lipgloss.NewStyle()
		s.Key = lipgloss.NewStyle()
		s.Value = lipgloss.NewStyle()
		s.Separator = lipgloss.NewStyle()
		s.Levels[clog.DebugLevel] = lipgloss.NewStyle()
		s.Levels[clog.InfoLevel] = lipgloss.NewStyle()
		s.Levels[clog.WarnLevel] = lipgloss.NewStyle()
		s.Levels[clog.ErrorLevel] = lipgloss.NewStyle()
		s.Levels[clog.FatalLevel] = lipgloss.NewStyle()
		s.Keys = map[string]lipgloss.Style{}
		s.Values = map[string]lipgloss.Style{}
		logger.SetStyles(s)
	} else {
		s := clog.DefaultStyles()
		s.Timestamp = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
		s.Caller = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Faint(true)
		logger.SetStyles(s)
	}
}

func isDebug() bool {
	return currentLogLevel >= levelDebug
}

func infoLog(format string, v ...any) {
	logger.Info(fmt.Sprintf(format, v...))
}

func debugLog(format string, v ...any) {
	if currentLogLevel >= levelDebug {
		logger.Debug(fmt.Sprintf(format, v...))
	}
}

func warnLog(format string, v ...any) {
	logger.Warn(fmt.Sprintf(format, v...))
}

func errLog(format string, v ...any) {
	logger.Error(fmt.Sprintf(format, v...))
}

func bannerLog(msg string) {
	if useColor {
		st := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
		logger.Info(st.Render("▸ " + msg))
	} else {
		logger.Info("▸ " + msg)
	}
}

func readyLog(name string, ms int64) {
	n := shortName(name)
	lat := fmt.Sprintf("%dms", ms)
	if useColor {
		nameSt := lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
		latCol := lipgloss.Color("42")
		if ms >= latCritMs {
			latCol = lipgloss.Color("196")
		} else if ms >= latWarnMs {
			latCol = lipgloss.Color("214")
		}
		latSt := lipgloss.NewStyle().Foreground(latCol).Bold(true)
		check := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✔")
		msg := fmt.Sprintf("%s Ready: %s [%s]", check, nameSt.Render(n), latSt.Render(lat))
		logger.Info(msg)
	} else {
		logger.Info(fmt.Sprintf("✔ Ready: %s [%s]", n, lat))
	}
}

func switchedLog(name string, ms int64) {
	n := shortName(name)
	lat := fmt.Sprintf("%dms", ms)
	if useColor {
		nameSt := lipgloss.NewStyle().Foreground(lipgloss.Color("86"))
		latSt := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
		icon := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("↻")
		logger.Info(fmt.Sprintf("%s Switched: %s [%s]", icon, nameSt.Render(n), latSt.Render(lat)))
	} else {
		logger.Info(fmt.Sprintf("↻ Switched: %s [%s]", n, lat))
	}
}

func shortName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		if idx := strings.LastIndex(s, "/"); idx >= 0 && idx+1 < len(s) {
			cand := strings.TrimSpace(s[idx+1:])
			if cand != "" {
				s = cand
			}
		}
		if idx := strings.LastIndex(s, "-"); idx >= 0 && len(s)-idx < 40 {
			cand := strings.TrimSpace(s[idx+1:])
			if cand != "" {
				s = cand
			}
		}
	}
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	r := []rune(s)
	if len(r) > shortNameMax {
		s = string(r[:shortNameMax]) + "…"
	}
	return s
}

func portOnly(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 && i+1 < len(addr) {
		return addr[i+1:]
	}
	return addr
}

func shortErr(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > shortErrMax {
		s = string(r[:shortErrMax]) + "…"
	}
	return s
}

func printSummaryTable(statuses []InstanceStatus) {
	if len(statuses) == 0 {
		return
	}
	re := lipgloss.NewRenderer(os.Stderr)
	baseStyle := re.NewStyle().Padding(0, 1)
	headerStyle := re.NewStyle().Foreground(lipgloss.Color("99")).Bold(true).Padding(0, 1)
	borderStyle := re.NewStyle().Foreground(lipgloss.Color("240"))
	if !useColor {
		baseStyle = lipgloss.NewStyle().Padding(0, 1)
		headerStyle = lipgloss.NewStyle().Bold(true).Padding(0, 1)
		borderStyle = lipgloss.NewStyle()
	}
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(borderStyle).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return headerStyle
			}
			if col == 4 {
				return baseStyle.Foreground(lipgloss.Color("42"))
			}
			if col == 5 {
				return baseStyle
			}
			if col == 1 {
				return baseStyle.Foreground(lipgloss.Color("86"))
			}
			return baseStyle
		}).
		Headers("#", "Name", "SOCKS", "HTTP", "Status", "Latency")
	if !useColor {
		t = table.New().
			Border(lipgloss.NormalBorder()).
			Headers("#", "Name", "SOCKS", "HTTP", "Status", "Latency")
	}
	rows := [][]string{}
	for _, s := range statuses {
		name := shortName(s.Name)
		stat := s.Status
		lat := fmt.Sprintf("%dms", s.LatMs)
		if s.Status != "ok" {
			lat = "—"
			if s.Error != "" {
				stat = fmt.Sprintf("%s (%s)", s.Status, shortErr(s.Error))
			}
		}
		if useColor {
			if stat == "ok" {
				stat = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true).Render(stat)
			} else {
				stat = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true).Render(stat)
			}
			lc := lipgloss.Color("42")
			if s.LatMs >= latCritMs {
				lc = lipgloss.Color("196")
			} else if s.LatMs >= latWarnMs {
				lc = lipgloss.Color("214")
			}
			lat = lipgloss.NewStyle().Foreground(lc).Render(lat)
		}
		rows = append(rows, []string{
			fmt.Sprintf("%d", s.Index),
			name,
			portOnly(s.SOCKS),
			portOnly(s.HTTP),
			stat,
			lat,
		})
	}
	t.Rows(rows...)
	title := "─ Instances ─"
	if useColor {
		title = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Faint(true).Render(title)
	}
	fmt.Fprintln(os.Stderr, title)
	fmt.Fprintln(os.Stderr, t.String())
}
