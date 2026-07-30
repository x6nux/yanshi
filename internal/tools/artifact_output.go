package tools

import "context"

type artifactOutput struct {
	Summary     string `json:"summary"`
	ArtifactRef string `json:"artifact_ref,omitempty"`
	Size        int64  `json:"size"`
	Degraded    bool   `json:"degraded,omitempty"`
}

func writeArtifactOrSpill(ctx context.Context, taskID, label, content string) artifactOutput {
	size := int64(len(content))
	if len(content) <= SpillThreshold {
		return artifactOutput{Summary: content, Size: size}
	}
	root := WorkRootFromContext(ctx)
	if root == "" {
		root = "."
	}
	if manager, ok := TaskManagerFromContext(ctx); ok {
		artifact, err := manager.WriteArtifact(ctx, taskID, label, []byte(content), root)
		if err == nil {
			return artifactOutput{Summary: artifact.Summary, ArtifactRef: artifact.ID, Size: artifact.Size}
		}
	}
	return artifactOutput{
		Summary:  "[degraded: task artifact manager unavailable; using temporary spillover]\n" + spillIfTooLong(ctx, label, content),
		Size:     size,
		Degraded: true,
	}
}
