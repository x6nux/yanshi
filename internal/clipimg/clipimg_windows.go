//go:build windows

package clipimg

import "context"

type platformReader struct{}

// ReadImage uses PowerShell to save clipboard bitmap as a temp PNG and read it
// back. Subprocess-only keeps CGO_ENABLED=0 compatible. No image → empty output
// → ok=false. The subprocess run goes through the commandOutput seam so tests
// can exercise this without a real clipboard; the default seam is identical to
// exec.CommandContext(...).Output().
func (platformReader) ReadImage(ctx context.Context) ([]byte, string, bool) {
	const script = `Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; $i = [System.Windows.Forms.Clipboard]::GetImage(); if ($i -ne $null) { $p = [System.IO.Path]::GetTempFileName() + '.png'; $i.Save($p, [System.Drawing.Imaging.ImageFormat]::Png); [System.IO.File]::ReadAllBytes($p); Remove-Item $p }`
	out, err := commandOutput(ctx, "powershell", "-NoProfile", "-Command", script)
	if err != nil || len(out) == 0 {
		return nil, "", false
	}
	return out, "png", true
}
