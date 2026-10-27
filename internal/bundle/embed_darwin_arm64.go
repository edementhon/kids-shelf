//go:build bundle && darwin && arm64

package bundle

import "embed"

//go:embed all:files/darwin_arm64
var embedded embed.FS

func init() { registerEmbedded(embedded) }
