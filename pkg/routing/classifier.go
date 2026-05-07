package routing

// Classifier evaluates a feature set and returns a complexity score in [0, 1].
// A higher score indicates a more complex task that benefits from a heavy model.
type Classifier interface {
	Score(f Features) float64
}

// RuleClassifier is the v2 implementation of the 3-tier routing system.
// It uses a weighted sum of structural signals with no external dependencies.
type RuleClassifier struct{}

// Score computes the complexity score for the given feature set.
// The returned value is in [0, 1]. Attachments short-circuit to 1.0.
func (c *RuleClassifier) Score(f Features) float64 {
	// Hard gate: multi-modal inputs always require the heavy model.
	if f.HasAttachments {
		return 1.0
	}

	var score float64

	// Token estimate — primary verbosity signal
	switch {
	case f.TokenEstimate > 200:
		score += 0.35
	case f.TokenEstimate > 50:
		score += 0.15
	}

	// Fenced code blocks
	if f.CodeBlockCount > 0 {
		score += 0.40
	}

	// Recent tool call density
	switch {
	case f.RecentToolCalls > 3:
		score += 0.25
	case f.RecentToolCalls > 0:
		score += 0.10
	}

	// Conversation depth
	if f.ConversationDepth > 10 {
		score += 0.10
	}

	// Code complexity score (0-0.45)
	if f.IsCodeLike {
		var codeScore float64
		
		switch {
		case f.CodeLines >= 20:
			codeScore += 0.20
		case f.CodeLines >= 5:
			codeScore += 0.10
		}

		switch {
		case f.FunctionBlocks >= 4:
			codeScore += 0.20
		case f.FunctionBlocks >= 1:
			codeScore += 0.10
		}

		switch {
		case f.ErrorLines >= 10:
			codeScore += 0.25
		case f.ErrorLines >= 1:
			codeScore += 0.15
		}

		if f.HasRuntimeWords {
			codeScore += 0.10
		}

		switch {
		case f.LangsHit >= 3:
			codeScore += 0.25
		case f.LangsHit >= 2:
			codeScore += 0.15
		}

		switch {
		case f.ProjectRefs >= 3:
			codeScore += 0.25
		case f.ProjectRefs >= 1:
			codeScore += 0.15
		}

		switch {
		case f.SystemConceptsCount >= 3:
			codeScore += 0.30
		case f.SystemConceptsCount == 2:
			codeScore += 0.20
		case f.SystemConceptsCount == 1:
			codeScore += 0.10
		}

		if f.HasTestingDeploy {
			codeScore += 0.05
			if f.SystemConceptsCount > 0 {
				codeScore += 0.05 // Bonus if combined with system concepts
			}
		}

		switch {
		case f.ConceptSpan >= 3:
			codeScore += 0.25
		case f.ConceptSpan == 2:
			codeScore += 0.15
		case f.ConceptSpan == 1:
			codeScore += 0.05
		}

		// Cap code complexity addition to 0.45 as designed
		if codeScore > 0.45 {
			codeScore = 0.45
		}
		score += codeScore
	}

	if score > 1.0 {
		score = 1.0
	}
	return score
}
