//go:build bundle && darwin && amd64

package bundle

import "embed"

//go:embed all:files/darwin_amd64
var embedded embed.FS

func init() { registerEmbedded(embedded) }
