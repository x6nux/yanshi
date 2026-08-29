//go:build windows

package tools

import "context"

func platformCapture(ctx context.Context) ([]byte, string, error) {
	// PowerShell + System.Drawing + System.Windows.Forms to capture primary screen.
	const script = `Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; $b = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds; $bmp = New-Object System.Drawing.Bitmap($b.Width, $b.Height); $g = [System.Drawing.Graphics]::FromImage($bmp); $g.CopyFromScreen($b.Location, [System.Drawing.Point]::Empty, $b.Size); $p = [System.IO.Path]::GetTempFileName() + '.png'; $bmp.Save($p, [System.Drawing.Imaging.ImageFormat]::Png); [System.IO.File]::ReadAllBytes($p); Remove-Item $p`
	out, err := captureCommand(ctx, "powershell", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return nil, "", err
	}
	return out, "png", nil
}
