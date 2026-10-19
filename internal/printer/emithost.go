package printer

import (
	"github.com/apsis-io/tsgo/internal/ast"
	"github.com/apsis-io/tsgo/internal/core"
	"github.com/apsis-io/tsgo/internal/tsoptions"
	"github.com/apsis-io/tsgo/internal/tspath"
)

// NOTE: EmitHost operations must be thread-safe
type EmitHost interface {
	Options() *core.CompilerOptions
	SourceFiles() []*ast.SourceFile
	ContentMapperExtensionRewrites() []tspath.ExtensionRewrite
	CaseSensitivity() tspath.CaseSensitivity
	CommonSourceDirectory() tspath.RootedDirectoryPath
	IsEmitBlocked(file tspath.RootedFilePath) bool
	WriteFile(fileName tspath.RootedFilePath, text string) error
	GetEmitModuleFormatOfFile(file ast.HasFileName) core.ModuleKind
	NewEmitResolver(emitContext *EmitContext) EmitResolver
	GetProjectReferenceFromSource(path tspath.PathKey) *tsoptions.SourceOutputAndProjectReference
	IsSourceFileFromExternalLibrary(file *ast.SourceFile) bool
}
