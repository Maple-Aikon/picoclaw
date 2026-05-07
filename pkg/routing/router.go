package routing

import (
	"github.com/sipeed/picoclaw/pkg/providers"
)

// defaultThreshold is used when the config threshold is zero or negative.
// At 0.35 a message needs at least one strong signal (code block, long text,
// or an attachment) before the heavy model is chosen.
const (
	defaultThreshold       = 0.25
	defaultMediumThreshold = 0.55
)

// RouterConfig holds the validated model routing settings.
type RouterConfig struct {
	// LightModel is the model_name used for very simple tasks (score < Threshold).
	LightModel string
	// MediumModel is the model_name used for moderate tasks (Threshold <= score < MediumThreshold).
	MediumModel string

	// Threshold is the complexity score cutoff for the light tier.
	Threshold float64
	// MediumThreshold is the complexity score cutoff for the medium tier.
	MediumThreshold float64
}

// Router selects the appropriate model tier for each incoming message.
type Router struct {
	cfg        RouterConfig
	classifier Classifier
}

// New creates a Router with the given config and the default RuleClassifier.
func New(cfg RouterConfig) *Router {
	cfg = normalizeConfig(cfg)
	return &Router{
		cfg:        cfg,
		classifier: &RuleClassifier{},
	}
}

// newWithClassifier creates a Router with a custom Classifier.
func newWithClassifier(cfg RouterConfig, c Classifier) *Router {
	cfg = normalizeConfig(cfg)
	return &Router{cfg: cfg, classifier: c}
}

// normalizeConfig ensures thresholds are within [0, 1] and Threshold <= MediumThreshold.
func normalizeConfig(cfg RouterConfig) RouterConfig {
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaultThreshold
	}
	if cfg.MediumThreshold <= 0 {
		cfg.MediumThreshold = defaultMediumThreshold
	}

	// Clamp to [0, 1]
	if cfg.Threshold > 1.0 {
		cfg.Threshold = 1.0
	}
	if cfg.MediumThreshold > 1.0 {
		cfg.MediumThreshold = 1.0
	}

	// Ensure Threshold <= MediumThreshold by swapping if inverted
	if cfg.Threshold > cfg.MediumThreshold {
		cfg.Threshold, cfg.MediumThreshold = cfg.MediumThreshold, cfg.Threshold
	}

	return cfg
}

// Tier represents the selected model tier.
type Tier int

const (
	TierLight Tier = iota
	TierMedium
	TierHeavy
)

// SelectModel returns the model name and tier to use for this conversation turn.
func (r *Router) SelectModel(
	msg string,
	history []providers.Message,
	primaryModel string,
) (model string, tier Tier, score float64) {
	features := ExtractFeatures(msg, history)
	score = r.classifier.Score(features)

	switch {
	case score < r.cfg.Threshold:
		return r.cfg.LightModel, TierLight, score
	case score < r.cfg.MediumThreshold && r.cfg.MediumModel != "":
		return r.cfg.MediumModel, TierMedium, score
	default:
		return primaryModel, TierHeavy, score
	}
}

// LightModel returns the configured light model name.
func (r *Router) LightModel() string {
	return r.cfg.LightModel
}

// MediumModel returns the configured medium model name.
func (r *Router) MediumModel() string {
	return r.cfg.MediumModel
}
