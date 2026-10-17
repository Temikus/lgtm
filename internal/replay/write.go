package replay

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"

	"github.com/Temikus/lgtm/internal/config"
)

var (
	calibrationHeader = regexp.MustCompile(`(?m)^[ \t]*\[[ \t]*calibration[ \t]*\][ \t]*(#.*)?$`)
	tableHeader       = regexp.MustCompile(`(?m)^[ \t]*\[`)
	commentOrBlank    = regexp.MustCompile(`^[ \t]*(#.*)?$`)
)

// SetCalibration replaces the [calibration] table in a config file's text,
// or appends one, and keeps every other byte. go-toml can't round-trip
// comments, so this edits text and then checks the result loads with c.
func SetCalibration(src []byte, c config.Calibration) ([]byte, error) {
	table := renderCalibration(c)
	var out []byte
	if loc := calibrationHeader.FindIndex(src); loc != nil {
		end := len(src)
		if next := tableHeader.FindIndex(src[loc[1]:]); next != nil {
			end = loc[1] + next[0]
		}
		end = keepTrailingComments(src, loc[0], end)
		out = append(append(append([]byte{}, src[:loc[0]]...), table...), src[end:]...)
	} else {
		out = bytes.TrimRight(src, "\n")
		if len(out) > 0 {
			out = append(out, "\n\n"...)
		}
		out = append(out, "# Written by `lgtm replay --write`. Enforce mode requires it.\n"...)
		out = append(out, table...)
	}

	got, err := config.Load(out)
	if err != nil {
		return nil, fmt.Errorf("the updated config doesn't load: %w", err)
	}
	if got.Calibration == nil || *got.Calibration != c {
		return nil, fmt.Errorf("the updated config doesn't contain the new calibration; edit %s by hand", config.Path)
	}
	return out, nil
}

// keepTrailingComments moves end back over comment and blank lines that sit
// just before the next table, since they belong to it.
func keepTrailingComments(src []byte, start, end int) int {
	lines := bytes.SplitAfter(src[start:end], []byte("\n"))
	cut := end
	for i := len(lines) - 1; i > 0; i-- {
		if !commentOrBlank.Match(bytes.TrimRight(lines[i], "\n")) {
			break
		}
		cut -= len(lines[i])
	}
	return cut
}

func renderCalibration(c config.Calibration) []byte {
	var b bytes.Buffer
	b.WriteString("[calibration]\n")
	fmt.Fprintf(&b, "model = %s\n", strconv.Quote(c.Model))
	if c.Snapshot != "" {
		fmt.Fprintf(&b, "snapshot = %s\n", strconv.Quote(c.Snapshot))
	}
	fmt.Fprintf(&b, "date = %s\n", c.Date)
	fmt.Fprintf(&b, "n_labelled = %d\n", c.NLabelled)
	fmt.Fprintf(&b, "approve_at = %s\n", num(c.ApproveAt))
	fmt.Fprintf(&b, "deny_at = %s\n", num(c.DenyAt))
	fmt.Fprintf(&b, "approve_precision = %s\n", num(c.ApprovePrecision))
	fmt.Fprintf(&b, "n_negative = %d\n", c.NNegative)
	fmt.Fprintf(&b, "false_approve = %s\n", num(c.FalseApprove))
	fmt.Fprintf(&b, "false_deny = %s\n", num(c.FalseDeny))
	return b.Bytes()
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
