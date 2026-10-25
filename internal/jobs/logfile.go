package jobs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Terminate returns a function that ends the log with "__EXIT__ <code>" on its own line. The
// last byte is read through the job's own fd (/proc/self/fd), so a rerun that replaced the path
// never confuses it.
func Terminate(log *os.File) func(code int) error {
	return func(code int) error {
		last := []byte{'\n'}
		if f, err := os.Open(fmt.Sprintf("/proc/self/fd/%d", log.Fd())); err == nil {
			if st, err := f.Stat(); err == nil && st.Size() > 0 {
				if _, err := f.ReadAt(last, st.Size()-1); err != nil && err != io.EOF {
					last[0] = 0
				}
			}
			f.Close()
		}
		var b bytes.Buffer
		if last[0] != '\n' {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "__EXIT__ %d\n", code)
		_, err := log.Write(b.Bytes())
		return err
	}
}

// LastExit returns the code of the last line that starts "__EXIT__ <int>". The log is read as
// text: a NUL is an ordinary byte, so "__EXIT__" after a NUL is mid-line.
func LastExit(b []byte) (int, bool) {
	code, found := 0, false
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "__EXIT__ ") {
			continue
		}
		f := strings.Fields(strings.TrimPrefix(line, "__EXIT__ "))
		if len(f) == 0 {
			continue
		}
		if n, err := strconv.Atoi(f[0]); err == nil {
			code, found = n, true
		}
	}
	return code, found
}

// Tail returns the last n lines (without trailing empty line).
func Tail(b []byte, n int) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
