package printer

import (
	"github.com/apsis-io/tsgo/internal/ast"
	"github.com/apsis-io/tsgo/internal/tspath"
)

type SourceFileMetaDataProvider interface {
	GetSourceFileMetaData(path tspath.PathKey) *ast.SourceFileMetaData
}
