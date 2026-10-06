package sourcemap

import (
	"github.com/apsis-io/tsgo/internal/core"
	"github.com/apsis-io/tsgo/internal/tspath"
)

type Source interface {
	Text() string
	FileName() tspath.RootedFilePath
	ECMALineMap() []core.TextPos
}
