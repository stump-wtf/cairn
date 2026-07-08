package sharetype

import (
	"strings"

	"github.com/joestump/cairn/internal/artifact"
)

// Registry keys for the built-in types beyond the generic file/gz/bundle keys
// declared on the aggregate. Their viewers and capture behavior are delivered by
// later stories (markdown/code/image = SPEC-0003; webhook = SPEC-0004;
// trajectory = SPEC-0005); this registry owns their identity and the anchor +
// previewability affordances that must not drift between viewer and validator.
const (
	KeyMarkdown   artifact.ShareType = "markdown"
	KeyCode       artifact.ShareType = "code"
	KeyImage      artifact.ShareType = "image"
	KeyWebhook    artifact.ShareType = "webhook"
	KeyTrajectory artifact.ShareType = "trajectory"
)

// simpleType is the common ShareType value: an identity, a badge, a
// previewability predicate over media type, and a set of legal anchors.
type simpleType struct {
	key     artifact.ShareType
	badge   string
	preview func(mediaType string) bool // nil => never previewable
	anchors []AnchorSpec
}

func (t simpleType) Key() artifact.ShareType { return t.key }
func (t simpleType) Badge() string           { return t.badge }
func (t simpleType) Anchors() []AnchorSpec   { return t.anchors }

func (t simpleType) PreviewableMedia(mediaType string) bool {
	return t.preview != nil && t.preview(mediaType)
}

func isText(mediaType string) bool {
	return strings.HasPrefix(strings.ToLower(mediaType), "text/")
}

func isImage(mediaType string) bool {
	return strings.HasPrefix(strings.ToLower(mediaType), "image/")
}

// anyMedia is used by types whose viewer exists independent of the body's media
// type (bundle tabs, webhook stream, trajectory waterfall).
func anyMedia(string) bool { return true }

// The generic file handler is the total-resolution floor: never previewable,
// only the (implicit) whole-artifact anchor. GZ is the same shape for gzip.
var (
	fileType = simpleType{key: artifact.TypeFile, badge: "FILE"}
	gzType   = simpleType{key: artifact.TypeGZ, badge: "GZ"}

	markdownType = simpleType{
		key:     KeyMarkdown,
		badge:   "MD",
		preview: isText,
		anchors: []AnchorSpec{both(AnchorMarkdownBlock), both(AnchorMarkdownBullet), both(AnchorSelection)},
	}
	codeType = simpleType{
		key:     KeyCode,
		badge:   "CODE",
		preview: isText,
		anchors: []AnchorSpec{both(AnchorCodeLine), both(AnchorSelection)},
	}
	imageType = simpleType{
		key:     KeyImage,
		badge:   "IMG",
		preview: isImage,
		anchors: []AnchorSpec{both(AnchorImageRegion)},
	}
	bundleType = simpleType{
		key:     artifact.TypeBundle,
		badge:   "BUNDLE",
		preview: anyMedia,
		anchors: []AnchorSpec{both(AnchorSelection)},
	}
	// Webhook requests are reactable but not comment-threaded — encoded as a
	// property of the type (SPEC-0002 REQ "Per-Type Anchor Affordances").
	webhookType = simpleType{
		key:     KeyWebhook,
		badge:   "HOOK",
		preview: anyMedia,
		anchors: []AnchorSpec{reactionOnly(AnchorWebhookRequest)},
	}
	trajectoryType = simpleType{
		key:     KeyTrajectory,
		badge:   "RUN",
		preview: anyMedia,
		anchors: []AnchorSpec{
			both(AnchorTrajectorySpan),
			both(AnchorTrajectoryTurn),
			both(AnchorTrajectoryToolCall),
			both(AnchorSelection),
		},
	}
)

// defaultRegistry is the process-wide registry, seeded with the built-in types.
// The generic file handler is both a registered type and the total-resolution
// fallback.
var defaultRegistry = newDefault()

func newDefault() *Registry {
	r := NewRegistry(fileType)
	for _, t := range []ShareType{
		fileType, gzType, markdownType, codeType, imageType,
		bundleType, webhookType, trajectoryType,
	} {
		r.Register(t)
	}
	return r
}

// Default returns the process-wide share-type registry.
func Default() *Registry { return defaultRegistry }

// Package-level conveniences over the default registry.

// Resolve resolves a key to its handler via the default registry (total).
func Resolve(key artifact.ShareType) ShareType { return defaultRegistry.Resolve(key) }

// DecidePreview decides effective type + previewability via the default registry.
func DecidePreview(declared artifact.ShareType, mediaType string, size, previewMax int64) (artifact.ShareType, bool) {
	return defaultRegistry.DecidePreview(declared, mediaType, size, previewMax)
}

// ValidateAnchor validates an anchor/kind pair via the default registry.
func ValidateAnchor(key artifact.ShareType, anchor Anchor, kind AnnotationKind) error {
	return defaultRegistry.ValidateAnchor(key, anchor, kind)
}

// AllowsAnchor reports anchor/kind legality via the default registry.
func AllowsAnchor(key artifact.ShareType, anchor Anchor, kind AnnotationKind) bool {
	return defaultRegistry.AllowsAnchor(key, anchor, kind)
}
