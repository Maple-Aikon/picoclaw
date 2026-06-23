package routing

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// lookbackWindow is the number of recent history entries scanned for tool calls.
const lookbackWindow = 6

// Features holds the structural signals extracted from a message and its session context.
type Features struct {
	TokenEstimate     int
	CodeBlockCount    int
	RecentToolCalls   int
	ConversationDepth int
	HasAttachments    bool

	// 3-tier routing features
	IsCodeLike          bool
	CodeLines           int
	FunctionBlocks      int
	ErrorLines          int
	HasRuntimeWords     bool
	LangsHit            int
	ProjectRefs         int
	SystemConceptsCount int
	HasTestingDeploy    bool
	ConceptSpan         int
}

// ExtractFeatures computes the structural feature vector for a message.
func ExtractFeatures(msg string, history []providers.Message) Features {
	f := Features{
		TokenEstimate:     estimateTokens(msg),
		CodeBlockCount:    countCodeBlocks(msg),
		RecentToolCalls:   countRecentToolCalls(history),
		ConversationDepth: len(history),
		HasAttachments:    hasAttachments(msg),
	}

	extractCodeFeatures(msg, &f)
	return f
}

func estimateTokens(msg string) int {
	total := utf8.RuneCountInString(msg)
	if total == 0 {
		return 0
	}
	cjk := 0
	for _, r := range msg {
		if r >= 0x2E80 && r <= 0x9FFF || r >= 0xF900 && r <= 0xFAFF || r >= 0xAC00 && r <= 0xD7AF {
			cjk++
		}
	}
	return cjk + (total-cjk)/4
}

func countCodeBlocks(msg string) int {
	n := strings.Count(msg, "```")
	return n / 2
}

func countRecentToolCalls(history []providers.Message) int {
	start := len(history) - lookbackWindow
	if start < 0 {
		start = 0
	}

	count := 0
	for _, msg := range history[start:] {
		if len(msg.ToolCalls) > 0 {
			count += len(msg.ToolCalls)
		}
	}
	return count
}

func hasAttachments(msg string) bool {
	lower := strings.ToLower(msg)

	if strings.Contains(lower, "data:image/") ||
		strings.Contains(lower, "data:audio/") ||
		strings.Contains(lower, "data:video/") {
		return true
	}

	mediaExts := []string{
		".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp",
		".mp3", ".wav", ".ogg", ".m4a", ".flac",
		".mp4", ".avi", ".mov", ".webm",
	}
	for _, ext := range mediaExts {
		if strings.Contains(lower, ext) {
			return true
		}
	}

	return false
}

// 3-tier feature extraction
var (
	langKeywords = map[string][]string{
		"go":     {"func ", "package ", "chan ", "goroutine", "defer "},
		"python": {"def ", "import ", "class ", "yield", "elif"},
		"js_ts":  {"function ", "const ", "let ", "=>", "await ", "interface "},
		"rust":   {"fn ", "impl ", "mut ", "match ", "pub "},
	}
	errorKeywords = []string{"traceback", "at ", "panic:", "exception in thread", "error:"}
	runtimeWords  = []string{"deadlock", "race", "memory leak", "sigsegv", "latency", "throughput"}
	projectFiles  = []string{"go.mod", "package.json", "dockerfile", "requirements.txt", "cargo.toml"}

	systemCategories = map[string][]string{
		"concurrency": {"mutex", "lock", "channel", "thread", "async"},
		"distributed": {"kafka", "redis", "grpc", "raft", "consensus"},
		"infra":       {"kubernetes", "k8s", "docker", "terraform", "aws"},
		"security":    {"oauth", "jwt", "tls", "encryption", "vuln"},
	}

	conceptCategories = map[string][]string{
		"algorithms":   {"sort", "search", "tree", "graph", "hash"},
		"ds":           {"array", "list", "map", "set", "queue"},
		"architecture": {"microservice", "monolith", "event-driven", "mvc"},
		"performance":  {"optimize", "cache", "profile", "benchmark"},
	}

	testingDeploy = []string{"test", "mock", "assert", "deploy", "ci/cd", "pipeline"}
)

func extractCodeFeatures(msg string, f *Features) {
	lower := strings.ToLower(msg)

	// IsCodeLike
	if f.CodeBlockCount > 0 || strings.Contains(msg, "`") {
		f.IsCodeLike = true
	} else if regexp.MustCompile(`\.[a-zA-Z0-9]+$|/[a-zA-Z0-9_-]+\.[a-zA-Z0-9]+`).MatchString(msg) {
		f.IsCodeLike = true
	} else {
		// lang hits
		totalLangHits := 0
		hasPunc := strings.ContainsAny(msg, "{};")
		for _, kwList := range langKeywords {
			hits := 0
			for _, kw := range kwList {
				if strings.Contains(lower, kw) {
					hits++
				}
			}
			if hits > 0 {
				f.LangsHit++
				totalLangHits += hits
			}
		}
		if totalLangHits >= 2 || (totalLangHits >= 1 && hasPunc) {
			f.IsCodeLike = true
		}

		// stack trace hits
		for _, kw := range errorKeywords {
			if strings.Contains(lower, kw) {
				f.IsCodeLike = true
				break
			}
		}
	}

	if !f.IsCodeLike {
		return
	}

	// CodeLines
	f.CodeLines = strings.Count(msg, "\n") + 1

	// FunctionBlocks
	f.FunctionBlocks = strings.Count(
		lower,
		"fn ",
	) + strings.Count(
		lower,
		"func ",
	) + strings.Count(
		lower,
		"def ",
	) + strings.Count(
		lower,
		"function ",
	)

	// ErrorLines
	for _, line := range strings.Split(lower, "\n") {
		for _, kw := range errorKeywords {
			if strings.Contains(line, kw) {
				f.ErrorLines++
				break
			}
		}
	}

	// Runtime words
	for _, kw := range runtimeWords {
		if strings.Contains(lower, kw) {
			f.HasRuntimeWords = true
			break
		}
	}

	// Project Refs
	for _, kw := range projectFiles {
		if strings.Contains(lower, kw) {
			f.ProjectRefs++
		}
	}

	// System Concepts
	for _, kwList := range systemCategories {
		for _, kw := range kwList {
			if strings.Contains(lower, kw) {
				f.SystemConceptsCount++
				break
			}
		}
	}

	// Testing/Deploy
	for _, kw := range testingDeploy {
		if strings.Contains(lower, kw) {
			f.HasTestingDeploy = true
			break
		}
	}

	// Concept Span
	for _, kwList := range conceptCategories {
		for _, kw := range kwList {
			if strings.Contains(lower, kw) {
				f.ConceptSpan++
				break
			}
		}
	}
}
