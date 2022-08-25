package graph

// This graph represents the set of files that the linker operates on. Each
// linker has a separate one of these graphs (there is one linker when code
// splitting is on, but one linker per entry point when code splitting is off).
//
// The input data to the linker constructor must be considered immutable because
// it's shared between linker invocations and is also stored in the cache for
// incremental builds.
//
// The linker constructor makes a shallow clone of the input data and is careful
// to pre-clone ahead of time the AST fields that it may modify. The Go language
// doesn't have any type system features for immutability so this has to be
// manually enforced. Please be careful.

import (
	"sort"
	"sync"

	"github.com/evanw/esbuild/internal/ast"
	"github.com/evanw/esbuild/internal/helpers"
	"github.com/evanw/esbuild/internal/js_ast"
	"github.com/evanw/esbuild/internal/logger"
	"github.com/evanw/esbuild/internal/runtime"
)

type entryPointKind uint8

const (
	entryPointNone entryPointKind = iota
	entryPointUserSpecified
	entryPointDynamicImport
)

type LinkerFile struct {
	// This holds all entry points that can reach this file. It will be used to
	// assign the parts in this file to a chunk.
	EntryBits helpers.BitSet

	// This is lazily-allocated because it's only needed if there are warnings
	// logged, which should be relatively rare.
	lazyLineColumnTracker *logger.LineColumnTracker

	InputFile InputFile

	// The minimum number of links in the module graph to get from an entry point
	// to this file
	DistanceFromEntryPoint uint32

	// If "entryPointKind" is not "entryPointNone", this is the index of the
	// corresponding entry point chunk.
	EntryPointChunkIndex uint32

	// This file is an entry point if and only if this is not "entryPointNone".
	// Note that dynamically-imported files are allowed to also be specified by
	// the user as top-level entry points, so some dynamically-imported files
	// may be "entryPointUserSpecified" instead of "entryPointDynamicImport".
	entryPointKind entryPointKind

	// This is true if this file has been marked as live by the tree shaking
	// algorithm.
	IsLive bool
}

func (f *LinkerFile) IsEntryPoint() bool {
	return f.entryPointKind != entryPointNone
}

func (f *LinkerFile) IsUserSpecifiedEntryPoint() bool {
	return f.entryPointKind == entryPointUserSpecified
}

// Note: This is not guarded by a mutex. Make sure this isn't called from a
// parallel part of the code.
func (f *LinkerFile) LineColumnTracker() *logger.LineColumnTracker {
	if f.lazyLineColumnTracker == nil {
		tracker := logger.MakeLineColumnTracker(&f.InputFile.Source)
		f.lazyLineColumnTracker = &tracker
	}
	return f.lazyLineColumnTracker
}

type EntryPoint struct {
	// This may be an absolute path or a relative path. If absolute, it will
	// eventually be turned into a relative path by computing the path relative
	// to the "outbase" directory. Then this relative path will be joined onto
	// the "outdir" directory to form the final output path for this entry point.
	OutputPath string

	// This is the source index of the entry point. This file must have a valid
	// entry point kind (i.e. not "none").
	SourceIndex uint32

	// Manually specified output paths are ignored when computing the default
	// "outbase" directory, which is computed as the lowest common ancestor of
	// all automatically generated output paths.
	OutputPathWasAutoGenerated bool
}

type LinkerGraph struct {
	Files       []LinkerFile
	entryPoints []EntryPoint
	Symbols     js_ast.SymbolMap

	// This is for cross-module inlining of TypeScript enum constants
	TSEnums map[js_ast.Ref]map[string]js_ast.TSEnumValue

	// This is for cross-module inlining of detected inlinable constants
	ConstValues map[js_ast.Ref]js_ast.ConstValue

	// We should avoid traversing all files in the bundle, because the linker
	// should be able to run a linking operation on a large bundle where only
	// a few files are needed (e.g. an incremental compilation scenario). This
	// holds all files that could possibly be reached through the entry points.
	// If you need to iterate over all files in the linking operation, iterate
	// over this array. This array is also sorted in a deterministic ordering
	// to help ensure deterministic builds (source indices are random).
	ReachableFiles []uint32

	// This maps from unstable source index to stable reachable file index. This
	// is useful as a deterministic key for sorting if you need to sort something
	// containing a source index (such as "js_ast.Ref" symbol references).
	StableSourceIndices []uint32
}

func CloneLinkerGraph(
	inputFiles []InputFile,
	reachableFiles []uint32,
	originalEntryPoints []EntryPoint,
	codeSplitting bool,
) LinkerGraph {
	entryPoints := append([]EntryPoint{}, originalEntryPoints...)
	symbols := js_ast.NewSymbolMap(len(inputFiles))
	files := make([]LinkerFile, len(inputFiles))

	// Mark all entry points so we don't add them again for import() expressions
	for _, entryPoint := range entryPoints {
		files[entryPoint.SourceIndex].entryPointKind = entryPointUserSpecified
	}

	// Clone various things since we may mutate them later. Do this in parallel
	// for a speedup (around ~2x faster for this function in the three.js
	// benchmark on a 6-core laptop).
	var dynamicImportEntryPoints []uint32
	var dynamicImportEntryPointsMutex sync.Mutex
	waitGroup := sync.WaitGroup{}
	waitGroup.Add(len(reachableFiles))
	stableSourceIndices := make([]uint32, len(inputFiles))
	for stableIndex, sourceIndex := range reachableFiles {
		// Create a way to convert source indices to a stable ordering
		stableSourceIndices[sourceIndex] = uint32(stableIndex)

		go func(sourceIndex uint32) {
			file := &files[sourceIndex]
			file.InputFile = inputFiles[sourceIndex]

			switch repr := file.InputFile.Repr.(type) {
			case *JSRepr:
				// Clone the representation
				{
					clone := *repr
					repr = &clone
					file.InputFile.Repr = repr
				}

				// Clone the symbol map
				fileSymbols := append([]js_ast.Symbol{}, repr.AST.Symbols...)
				symbols.SymbolsForSource[sourceIndex] = fileSymbols
				repr.AST.Symbols = nil

				// Clone the parts
				repr.AST.Parts = append([]js_ast.Part{}, repr.AST.Parts...)
				for i := range repr.AST.Parts {
					part := &repr.AST.Parts[i]
					clone := make(map[js_ast.Ref]js_ast.SymbolUse, len(part.SymbolUses))
					for ref, uses := range part.SymbolUses {
						clone[ref] = uses
					}
					part.SymbolUses = clone
				}

				// Clone the import records
				repr.AST.ImportRecords = append([]ast.ImportRecord{}, repr.AST.ImportRecords...)

				// Add dynamic imports as additional entry points if code splitting is active
				if codeSplitting {
					for importRecordIndex := range repr.AST.ImportRecords {
						if record := &repr.AST.ImportRecords[importRecordIndex]; record.SourceIndex.IsValid() && record.Kind == ast.ImportDynamic {
							dynamicImportEntryPointsMutex.Lock()
							dynamicImportEntryPoints = append(dynamicImportEntryPoints, record.SourceIndex.GetIndex())
							dynamicImportEntryPointsMutex.Unlock()

							// Remove import assertions for dynamic imports of additional
							// entry points so that they don't mess with the run-time behavior.
							// For example, "import('./foo.json', { assert: { type: 'json' } })"
							// will likely be converted into an import of a JavaScript file and
							// leaving the import assertion there will prevent it from working.
							record.Assertions = nil
						}
					}
				}

				// Clone the import map
				namedImports := make(map[js_ast.Ref]js_ast.NamedImport, len(repr.AST.NamedImports))
				for k, v := range repr.AST.NamedImports {
					namedImports[k] = v
				}
				repr.AST.NamedImports = namedImports

				// Clone the export map
				resolvedExports := make(map[string]ExportData)
				for alias, name := range repr.AST.NamedExports {
					resolvedExports[alias] = ExportData{
						Ref:         name.Ref,
						SourceIndex: sourceIndex,
						NameLoc:     name.AliasLoc,
					}
				}

				// Clone the top-level scope so we can generate more variables
				{
					new := &js_ast.Scope{}
					*new = *repr.AST.ModuleScope
					new.Generated = append([]js_ast.Ref{}, new.Generated...)
					repr.AST.ModuleScope = new
				}

				// Also associate some default metadata with the file
				repr.Meta.ResolvedExports = resolvedExports
				repr.Meta.IsProbablyTypeScriptType = make(map[js_ast.Ref]bool)
				repr.Meta.ImportsToBind = make(map[js_ast.Ref]ImportData)

			case *CSSRepr:
				// Clone the representation
				{
					clone := *repr
					repr = &clone
					file.InputFile.Repr = repr
				}

				// Clone the import records
				repr.AST.ImportRecords = append([]ast.ImportRecord{}, repr.AST.ImportRecords...)
			}

			// All files start off as far as possible from an entry point
			file.DistanceFromEntryPoint = ^uint32(0)
			waitGroup.Done()
		}(sourceIndex)
	}
	waitGroup.Wait()

	// Process dynamic entry points after merging control flow again
	stableEntryPoints := make([]int, 0, len(dynamicImportEntryPoints))
	for _, sourceIndex := range dynamicImportEntryPoints {
		if otherFile := &files[sourceIndex]; otherFile.entryPointKind == entryPointNone {
			stableEntryPoints = append(stableEntryPoints, int(stableSourceIndices[sourceIndex]))
			otherFile.entryPointKind = entryPointDynamicImport
		}
	}

	// Make sure to add dynamic entry points in a deterministic order
	sort.Ints(stableEntryPoints)
	for _, stableIndex := range stableEntryPoints {
		entryPoints = append(entryPoints, EntryPoint{SourceIndex: reachableFiles[stableIndex]})
	}

	// Do a final quick pass over all files
	var tsEnums map[js_ast.Ref]map[string]js_ast.TSEnumValue
	var constValues map[js_ast.Ref]js_ast.ConstValue
	bitCount := uint(len(entryPoints))
	for _, sourceIndex := range reachableFiles {
		file := &files[sourceIndex]

		// Allocate the entry bit set now that the number of entry points is known
		file.EntryBits = helpers.NewBitSet(bitCount)

		// Merge TypeScript enums together into one big map. There likely aren't
		// too many enum definitions relative to the overall size of the code so
		// it should be fine to just merge them together in serial.
		if repr, ok := file.InputFile.Repr.(*JSRepr); ok && repr.AST.TSEnums != nil {
			if tsEnums == nil {
				tsEnums = make(map[js_ast.Ref]map[string]js_ast.TSEnumValue)
			}
			for ref, enum := range repr.AST.TSEnums {
				tsEnums[ref] = enum
			}
		}

		// Also merge const values into one big map as well
		if repr, ok := file.InputFile.Repr.(*JSRepr); ok && repr.AST.ConstValues != nil {
			if constValues == nil {
				constValues = make(map[js_ast.Ref]js_ast.ConstValue)
			}
			for ref, value := range repr.AST.ConstValues {
				constValues[ref] = value
			}
		}
	}

	return LinkerGraph{
		Symbols:             symbols,
		TSEnums:             tsEnums,
		ConstValues:         constValues,
		entryPoints:         entryPoints,
		Files:               files,
		ReachableFiles:      reachableFiles,
		StableSourceIndices: stableSourceIndices,
	}
}

// Prevent packages that depend on us from adding or removing entry points
func (g *LinkerGraph) EntryPoints() []EntryPoint {
	return g.entryPoints
}

func (g *LinkerGraph) AddPartToFile(sourceIndex uint32, part js_ast.Part) uint32 {
	// Invariant: this map is never null
	if part.SymbolUses == nil {
		part.SymbolUses = make(map[js_ast.Ref]js_ast.SymbolUse)
	}

	repr := g.Files[sourceIndex].InputFile.Repr.(*JSRepr)
	partIndex := uint32(len(repr.AST.Parts))
	repr.AST.Parts = append(repr.AST.Parts, part)

	// Invariant: the parts for all top-level symbols can be found in the file-level map
	for _, declaredSymbol := range part.DeclaredSymbols {
		if declaredSymbol.IsTopLevel {
			// Check for an existing overlay
			partIndices, ok := repr.Meta.TopLevelSymbolToPartsOverlay[declaredSymbol.Ref]

			// If missing, initialize using the original values from the parser
			if !ok {
				partIndices = append(partIndices, repr.AST.TopLevelSymbolToPartsFromParser[declaredSymbol.Ref]...)
			}

			// Add this part to the overlay
			partIndices = append(partIndices, partIndex)
			if repr.Meta.TopLevelSymbolToPartsOverlay == nil {
				repr.Meta.TopLevelSymbolToPartsOverlay = make(map[js_ast.Ref][]uint32)
			}
			repr.Meta.TopLevelSymbolToPartsOverlay[declaredSymbol.Ref] = partIndices
		}
	}

	return partIndex
}

func (g *LinkerGraph) GenerateNewSymbol(sourceIndex uint32, kind js_ast.SymbolKind, originalName string) js_ast.Ref {
	sourceSymbols := &g.Symbols.SymbolsForSource[sourceIndex]

	ref := js_ast.Ref{
		SourceIndex: sourceIndex,
		InnerIndex:  uint32(len(*sourceSymbols)),
	}

	*sourceSymbols = append(*sourceSymbols, js_ast.Symbol{
		Kind:         kind,
		OriginalName: originalName,
		Link:         js_ast.InvalidRef,
	})

	generated := &g.Files[sourceIndex].InputFile.Repr.(*JSRepr).AST.ModuleScope.Generated
	*generated = append(*generated, ref)
	return ref
}

func (g *LinkerGraph) GenerateSymbolImportAndUse(
	sourceIndex uint32,
	partIndex uint32,
	ref js_ast.Ref,
	useCount uint32,
	sourceIndexToImportFrom uint32,
) {
	if useCount == 0 {
		return
	}

	repr := g.Files[sourceIndex].InputFile.Repr.(*JSRepr)
	part := &repr.AST.Parts[partIndex]

	// Mark this symbol as used by this part
	use := part.SymbolUses[ref]
	use.CountEstimate += useCount
	part.SymbolUses[ref] = use

	// Uphold invariants about the CommonJS "exports" and "module" symbols
	if ref == repr.AST.ExportsRef {
		repr.AST.UsesExportsRef = true
	}
	if ref == repr.AST.ModuleRef {
		repr.AST.UsesModuleRef = true
	}

	// Track that this specific symbol was imported
	if sourceIndexToImportFrom != sourceIndex {
		repr.Meta.ImportsToBind[ref] = ImportData{
			SourceIndex: sourceIndexToImportFrom,
			Ref:         ref,
		}
	}

	// Pull in all parts that declare this symbol
	targetRepr := g.Files[sourceIndexToImportFrom].InputFile.Repr.(*JSRepr)
	for _, partIndex := range targetRepr.TopLevelSymbolToParts(ref) {
		part.Dependencies = append(part.Dependencies, js_ast.Dependency{
			SourceIndex: sourceIndexToImportFrom,
			PartIndex:   partIndex,
		})
	}
}

func (g *LinkerGraph) GenerateRuntimeSymbolImportAndUse(
	sourceIndex uint32,
	partIndex uint32,
	name string,
	useCount uint32,
) {
	if useCount == 0 {
		return
	}

	runtimeRepr := g.Files[runtime.SourceIndex].InputFile.Repr.(*JSRepr)
	ref := runtimeRepr.AST.NamedExports[name].Ref
	g.GenerateSymbolImportAndUse(sourceIndex, partIndex, ref, useCount, runtime.SourceIndex)
}
