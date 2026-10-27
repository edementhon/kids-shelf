//go:build bundle && linux && arm64

package bundle

import "embed"

//go:embed all:files/linux_arm64
var embedded embed.FS

func init() { registerEmbedded(embedded) }
