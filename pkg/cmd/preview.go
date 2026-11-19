package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/cli/output"
	"helm.sh/helm/v4/pkg/kube"
)

type upgradePreviewWriter struct {
	preview *action.UpgradePreview
}

func (w *upgradePreviewWriter) WriteJSON(out io.Writer) error {
	return output.EncodeJSON(out, w.machinePreview())
}

func (w *upgradePreviewWriter) WriteYAML(out io.Writer) error {
	return output.EncodeYAML(out, w.machinePreview())
}

func (w *upgradePreviewWriter) WriteTable(out io.Writer) error {
	if w.preview == nil {
		return nil
	}

	var buffer bytes.Buffer
	fmt.Fprintf(&buffer, "RELEASE: %s\n", w.preview.Release)
	fmt.Fprintf(&buffer, "NAMESPACE: %s\n", w.preview.Namespace)
	fmt.Fprintf(&buffer, "REVISION: %d\n", w.preview.Revision)
	fmt.Fprintf(&buffer, "SUMMARY: %s\n", formatPreviewSummary(w.preview.Summary))
	if w.preview.SummaryOnly {
		_, err := out.Write(buffer.Bytes())
		return err
	}

	for _, resource := range w.preview.Resources {
		fmt.Fprintf(&buffer, "\n%s %s\n", strings.ToUpper(string(resource.Operation)), previewResourceName(resource))
		if resource.Reason != "" {
			fmt.Fprintf(&buffer, "  note: %s\n", resource.Reason)
		}
		for _, field := range resource.Fields {
			switch field.Operation {
			case kube.DiffOperationAdd:
				fmt.Fprintf(&buffer, "  + %s = %s\n", field.Path, formatPreviewValue(field.Desired))
			case kube.DiffOperationRemove:
				fmt.Fprintf(&buffer, "  - %s = %s\n", field.Path, formatPreviewValue(field.Current))
			case kube.DiffOperationType:
				fmt.Fprintf(
					&buffer,
					"  ! %s: %s (%s) ~ %s (%s), semantically equal\n",
					field.Path,
					formatPreviewValue(field.Current),
					field.CurrentType,
					formatPreviewValue(field.Desired),
					field.DesiredType,
				)
			default:
				fmt.Fprintf(
					&buffer,
					"  ~ %s: %s -> %s\n",
					field.Path,
					formatPreviewValue(field.Current),
					formatPreviewValue(field.Desired),
				)
			}
		}
	}

	if w.preview.Summary.Truncated {
		fmt.Fprintf(
			&buffer,
			"\nTRUNCATED: adjust --preview-limit and --preview-field-limit to expand the output.\n",
		)
	}
	_, err := out.Write(buffer.Bytes())
	return err
}

func (w *upgradePreviewWriter) machinePreview() *action.UpgradePreview {
	if w.preview == nil {
		return nil
	}
	if !w.preview.SummaryOnly {
		return w.preview
	}
	preview := *w.preview
	preview.Resources = nil
	return &preview
}

func formatPreviewSummary(summary kube.DiffSummary) string {
	return fmt.Sprintf(
		"total=%d added=%d updated=%d deleted=%d incomparable=%d fieldChanges=%d typeMismatches=%d omitted=%d truncated=%t",
		summary.Total,
		summary.Added,
		summary.Updated,
		summary.Deleted,
		summary.Incomparable,
		summary.FieldChanges,
		summary.TypeMismatches,
		summary.OmittedResources,
		summary.Truncated,
	)
}

func previewResourceName(resource kube.ResourceDiff) string {
	name := fmt.Sprintf("%s %s", resource.Kind, resource.Name)
	if resource.Namespace != "" {
		name = fmt.Sprintf("%s %s/%s", resource.Kind, resource.Namespace, resource.Name)
	}
	if resource.APIVersion != "" {
		name += " (" + resource.APIVersion + ")"
	}
	return name
}

func formatPreviewValue(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	const maxValueLength = 240
	if len(raw) > maxValueLength {
		return string(raw[:maxValueLength]) + "..."
	}
	return string(raw)
}
