//go:build bundle && windows && amd64

package bundle

import "embed"

//go:embed all:files/windows_amd64
var embedded embed.FS

func init() { registerEmbedded(embedded) }
