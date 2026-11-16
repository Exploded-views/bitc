package plugin

import "os"

// shimSpec is how the adapter starts the `bitc __plugin-exec` shim for one plugin process.
type shimSpec struct {
	exe   string     // the bitc binary that runs the shim
	args  []string   // argv of the shim, argv[0] included
	extra []*os.File // fd 3 onwards of the shim
	child *os.File   // a read end the parent closes once the shim started (nil if none)
	// lifeline is the write end the parent holds for the life of the process: when bitc dies it
	// closes and the darwin shim takes the plugin's group down (nil on Linux, which has Pdeathsig)
	lifeline *os.File
}

func (s shimSpec) closeChildEnds() {
	if s.child != nil {
		_ = s.child.Close()
	}
}

func (s shimSpec) closeLifeline() {
	if s.lifeline != nil {
		_ = s.lifeline.Close()
	}
}
