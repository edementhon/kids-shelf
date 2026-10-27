//go:build bundle && linux && amd64

package bundle

import "embed"

//go:embed all:files/linux_amd64
var embedded embed.FS

func init() { registerEmbedded(embedded) }
