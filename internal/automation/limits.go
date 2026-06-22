package automation

import (
	"io"
	"strings"
)

// Bounds that keep a hostile or runaway definition/selector from exhausting host
// resources. They are generous for real automations and tight against abuse; the
// HTTP layer also caps the request body, so these are defense in depth.
const (
	maxDocumentBytes  = 256 << 10 // a single automation.v1 document
	maxSteps          = 500       // total steps across all nesting
	maxStepDepth      = 16        // for_each/parallel nesting depth
	maxSelectorDepth  = 32        // path segments in one reference (a.b.c -> 3)
	maxSelectorLen    = 1024      // bytes in one reference/template string
	maxTemplateExpand = 256       // ${...} expansions in one template
	maxExpandedBytes  = 256 << 10 // total bytes a single template may expand to
	maxNameLen        = 200       // automation name
	maxDescriptionLen = 4000      // automation description
	maxStringFieldLen = 8000      // any other model/user-supplied string field
	maxListLen        = 256       // any allow-list / refs slice
)

// boundedReader caps how many bytes Parse will read from a document, so an
// over-large blob is rejected by the decoder rather than buffered whole.
func boundedReader(data []byte) io.Reader {
	return io.LimitReader(strings.NewReader(string(data)), maxDocumentBytes+1)
}
