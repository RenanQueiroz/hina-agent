package asr

// Decode-time context biasing for the agent name (research-findings B3): a
// SentencePiece-token trie over the configured name + aliases, with a
// per-hypothesis pointer. After the joint emits its logits, tokens that continue
// an active trie path get an additive boost, so a name the model would otherwise
// mis-hear ("Hina" -> "Nina"/"Tina") is preferred. This is pure decode-time,
// graph-independent, and quantization-independent — no retrain, no ONNX change —
// and the trie is rebuilt at runtime whenever the name changes.
//
// Equivalence note: greedy decode takes argmax over the logits. log_softmax is
// logits minus a per-frame constant, so argmax(logits + boost) ==
// argmax(log_softmax(logits) + boost). Adding the boost to raw logits is
// therefore identical to NeMo's log-prob-domain context_score for selection —
// so the calibrated starting point (context_score≈1.0, depth_scaling≈2.0)
// transfers directly.

// DefaultContextScore / DefaultDepthScaling are the starting bias parameters
// (research-findings B3). The first matched token of a phrase gets
// +contextScore; deeper tokens get +contextScore·depthScaling, committing harder
// once a phrase is underway. Tune on the name-recognition fixture (Phase 6).
const (
	DefaultContextScore = 1.0
	DefaultDepthScaling = 2.0
)

// biasNode is one node of the immutable token trie. children is keyed by token
// id; terminal marks the end of a configured phrase. The trie is built once and
// read-only thereafter, so a *biasNode cursor can be threaded through a decode
// without locking.
type biasNode struct {
	children map[int]*biasNode
	depth    int
	terminal bool
}

// BiasContext is the compiled, immutable name-biasing trie plus its boost
// parameters. A zero/empty context (no phrases) is valid and applies no boost,
// so callers never need a nil check. Safe for concurrent read use.
type BiasContext struct {
	root         *biasNode
	contextScore float32
	depthScaling float32
	empty        bool
}

// NewBiasContext compiles phrases (each a token-id sequence, e.g. from
// Tokenizer.Encode of the name and its aliases) into a boosting trie. Empty or
// nil phrases yield an inert context. Non-positive params fall back to the
// defaults.
func NewBiasContext(phrases [][]int, contextScore, depthScaling float64) *BiasContext {
	if contextScore <= 0 {
		contextScore = DefaultContextScore
	}
	if depthScaling <= 0 {
		depthScaling = DefaultDepthScaling
	}
	root := &biasNode{children: map[int]*biasNode{}}
	count := 0
	for _, ph := range phrases {
		if len(ph) == 0 {
			continue
		}
		node := root
		for _, tok := range ph {
			next := node.children[tok]
			if next == nil {
				next = &biasNode{children: map[int]*biasNode{}, depth: node.depth + 1}
				node.children[tok] = next
			}
			node = next
		}
		node.terminal = true
		count++
	}
	return &BiasContext{
		root:         root,
		contextScore: float32(contextScore),
		depthScaling: float32(depthScaling),
		empty:        count == 0,
	}
}

// Enabled reports whether the context has any biased phrases.
func (b *BiasContext) Enabled() bool { return b != nil && !b.empty }

// cursor returns the trie root, the starting decode position.
func (b *BiasContext) cursor() *biasNode {
	if b == nil {
		return nil
	}
	return b.root
}

// apply adds the bias boost in-place to logits for every token that continues
// the active path at node. The boost is +contextScore for a phrase's first token
// (depth 0 -> child depth 1) and +contextScore·depthScaling for deeper tokens,
// so an in-progress phrase carries through. No-op for an inert context or a nil
// node. Out-of-range child ids are skipped defensively.
func (b *BiasContext) apply(logits []float32, node *biasNode) {
	if b == nil || b.empty || node == nil {
		return
	}
	for tok, child := range node.children {
		if tok < 0 || tok >= len(logits) {
			continue
		}
		boost := b.contextScore
		if child.depth >= 2 {
			boost = b.contextScore * b.depthScaling
		}
		logits[tok] += boost
	}
}

// advance returns the trie position after emitting token tok from node. If tok
// continues the active path, advance into that child (and, if it's a leaf with no
// further children, reset to root — the phrase completed). Otherwise reset to
// root, then re-enter if tok itself starts a phrase, so back-to-back names and a
// name beginning mid-stream are both caught. A nil/inert context returns nil.
func (b *BiasContext) advance(node *biasNode, tok int) *biasNode {
	if b == nil || b.empty {
		return nil
	}
	if node != nil {
		if next, ok := node.children[tok]; ok {
			if len(next.children) == 0 {
				return b.root // completed phrase; start fresh
			}
			return next
		}
	}
	// Path broke: restart from root, entering a new phrase if tok begins one.
	if next, ok := b.root.children[tok]; ok {
		if len(next.children) == 0 {
			return b.root
		}
		return next
	}
	return b.root
}
