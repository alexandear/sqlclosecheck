package analyzer

import (
	"flag"
	"go/types"
	"log"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
)

const (
	rowsName      = "Rows"
	stmtName      = "Stmt"
	namedStmtName = "NamedStmt"
	closeMethod   = "Close"
)

type action uint8

const (
	// target not handled (what does handled mean?))
	actionUnhandled action = iota
	// target handled (what does handled mean?)
	actionHandled
	// target returned by function
	actionReturned
	// target passed to function
	actionPassed
	// target closed (desired outcome)
	actionClosed
	// target unvalledcall (?)
	actionUnvaluedCall
	// target unvalued defer (?)
	actionUnvaluedDefer
	// noop (?)
	actionNoOp
)

var (
	sqlPackages = []string{
		"database/sql",
		"github.com/jmoiron/sqlx",
		"github.com/jackc/pgx/v5",
		"github.com/jackc/pgx/v5/pgxpool",
	}
)

// legacyAnalyzer is an analyzer checks for unclosed rows/stmts.
// This analyzer has organically grown and is not does not implement a coherent
// approach to checking for unclosed rows/stmts. Over time this analyzer will be
// improved/refactored or replaced.
type legacyAnalyzer struct{}

func NewLegacyAnalyzer() *analysis.Analyzer {
	analyzer := &legacyAnalyzer{}
	flags := flag.NewFlagSet("legacyAnalyzer", flag.ExitOnError)
	return newAnalyzer(analyzer.Run, flags)
}

// Run implements the main analysis pass. It iterates over all functions,
// blocks, and instructions looking for rows/stmts provided by supported
// packages. If a rows/stmt is found, it scans the referrers for a close call.
// If a close call is not found, a report is generated.
func (a *legacyAnalyzer) Run(pass *analysis.Pass) (interface{}, error) {
	pssa, ok := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	if !ok {
		return nil, nil
	}

	// Build list of types we are looking for
	targetTypes := getTargetTypes(pssa, sqlPackages)

	// If non of the types are found, skip
	if len(targetTypes) == 0 {
		return nil, nil
	}

	funcs := pssa.SrcFuncs
	for _, f := range funcs {
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				// Check if instruction is call that returns a target pointer type
				targetValues := getClosableValues(b, i, targetTypes)
				if len(targetValues) == 0 {
					continue
				}

				// For each found target check if they are closed and deferred
				for _, targetValue := range targetValues {
					log.Printf("target value: %v", *targetValue.value)

					refs := (*targetValue.value).Referrers()

					isClosed := isClosed(refs, targetTypes)
					if !isClosed {
						pass.Reportf((targetValue.instr).Pos(), "Rows/Stmt/NamedStmt was not closed")
					}

					checkDeferred(pass, refs, targetTypes, false)
				}
			}
		}
	}

	return nil, nil
}

// isClosed checks if the target is closed and returns true if it is.
// Each instruction is checked to see if it's a close call, if it is then
// we are done and true is returned.
func isClosed(refs *[]ssa.Instruction, targetTypes []any) bool {
	// numInstrs := len(*refs)
	for idx, ref := range *refs {
		log.Printf("===== checking ref for close: %d %s", idx, ref.String())

		action := getAction(ref, targetTypes)
		log.Printf("action: %d", action)

		switch action {
		case actionClosed: // desired outcome
			return true
		case actionHandled: // what does handled mean? how is it different than closed?
			return true
		case actionReturned: // should follow the return value to see if it is closed
			continue
		case actionUnhandled:
			continue
		case actionPassed: // should follow the passed value to see if it is closed
		// Pass to another function/method, should check what that function/method does
		// TODO check if the function passed to handles it
		// blockRefs := ref.Block().Instrs
		// log.Printf("blockRefs: %v", blockRefs)

		// This is probably not needed, what the func/method does should be checked
		// if there isn't any instructions left, then the result of this should be considered
		// for this branch
		//
		// // Passed and not used after
		// if numInstrs == idx+1 {
		// 	log.Printf("Passed and not used after")
		// 	return true
		// }
		default:
			log.Printf("unexpected action: %d", action)
		}
	}

	return false
}

// getAction returns the action taken on the target instruction.
func getAction(instr ssa.Instruction, targetTypes []any) action {
	log.Printf("getAction: %s %v", instr.String(), instr.Block().Instrs)

	switch instr := instr.(type) {
	case *ssa.Defer:
		log.Printf("defer: %s", instr.Call.Value.Name())

		if instr.Call.Value != nil {
			name := instr.Call.Value.Name()
			if name == closeMethod {
				return actionClosed
			}
		}

		if instr.Call.Method != nil {
			name := instr.Call.Method.Name()
			if name == closeMethod {
				return actionClosed
			}
		} else if instr.Call.Value != nil {
			// If it is a deferred function, go further down the call chain
			if f, ok := instr.Call.Value.(*ssa.Function); ok {
				for _, b := range f.Blocks {
					if isClosed(&b.Instrs, targetTypes) {
						return actionClosed
					}
				}
			}
		}

		return actionUnvaluedDefer
	case *ssa.Call:
		// function/method call
		log.Printf("Call: %s %s %s", instr.Call.Value.Name(), instr.Call.Value.String(), instr.Call.Value.Type())

		if instr.Call.Value == nil {
			return actionUnvaluedCall
		}

		isTarget := false
		staticCallee := instr.Call.StaticCallee()
		if staticCallee != nil {
			receiver := instr.Call.StaticCallee().Signature.Recv()
			if receiver != nil {
				log.Printf("Receiver: %s", receiver.Type().String())
				isTarget = isTargetType(receiver.Type(), targetTypes)
			}
		} else {
			isTarget = isTargetType(instr.Call.Value.Type(), targetTypes)
		}

		log.Printf("isTarget: %v %s", isTarget, instr.Call.Value.Name())

		name := instr.Call.Value.Name()
		if isTarget && name == closeMethod {
			return actionClosed
		}

		if !isTarget {
			log.Printf("%v is not a target", instr.Call.Value.Name())
			staticCallee := instr.Common().StaticCallee()
			if staticCallee == nil {
				return actionUnhandled
			}

			blocks := staticCallee.Blocks
			log.Printf("Blocks: %v", blocks)

			// iterate blocks and check if any of them close the target
			for _, b := range blocks {
				if isClosed(&b.Instrs, targetTypes) {
					return actionClosed
				}
			}
		}

		return actionUnhandled
	case *ssa.Phi:
		log.Printf("Phi: %s", instr.String())
		return actionPassed
	case *ssa.MakeInterface:
		log.Printf("MakeInterface: %s", instr.String())
		return actionPassed
	case *ssa.Store:
		log.Printf("Store: %s", instr.String())

		// A Row/Stmt is stored in a struct, which may be closed later
		// by a different flow.
		if _, ok := instr.Addr.(*ssa.FieldAddr); ok {
			return actionReturned
		}

		if len(*instr.Addr.Referrers()) == 0 {
			return actionNoOp
		}

		for _, aRef := range *instr.Addr.Referrers() {
			if c, ok := aRef.(*ssa.MakeClosure); ok {
				if f, ok := c.Fn.(*ssa.Function); ok {
					for _, b := range f.Blocks {
						if isClosed(&b.Instrs, targetTypes) {
							return actionHandled
						}
					}
				}
			}
		}
	case *ssa.UnOp:
		log.Printf("UnOp: %s", instr.String())

		instrType := instr.Type()
		for _, targetType := range targetTypes {
			var tt types.Type

			switch t := targetType.(type) {
			case *types.Pointer:
				tt = t
			case *types.Named:
				tt = t
			default:
				continue
			}

			if types.Identical(instrType, tt) {
				if isClosed(instr.Referrers(), targetTypes) {
					return actionHandled
				}
			}
		}
	case *ssa.FieldAddr:
		log.Printf("FieldAddr: %s", instr.String())

		if isClosed(instr.Referrers(), targetTypes) {
			return actionHandled
		}
	case *ssa.Return:
		log.Printf("Return: %s", instr.Results)

		// Check if the return value is a target type
		if len(instr.Results) != 0 {
			for _, result := range instr.Results {
				resultType := result.Type()
				for _, targetType := range targetTypes {
					var tt types.Type

					switch t := targetType.(type) {
					case *types.Pointer:
						tt = t
					case *types.Named:
						tt = t
					default:
						continue
					}

					if types.Identical(resultType, tt) {
						return actionReturned
					}
				}
			}
		}
	}

	return actionUnhandled
}

func checkDeferred(pass *analysis.Pass, instrs *[]ssa.Instruction, targetTypes []any, inDefer bool) {
	for _, instr := range *instrs {
		switch instr := instr.(type) {
		case *ssa.Defer:
			if instr.Call.Value != nil && instr.Call.Value.Name() == closeMethod {
				return
			}

			if instr.Call.Method != nil && instr.Call.Method.Name() == closeMethod {
				return
			}
		case *ssa.Call:
			if instr.Call.Value != nil && instr.Call.Value.Name() == closeMethod {
				if !inDefer {
					pass.Reportf(instr.Pos(), "Close should use defer")
				}

				return
			}
		case *ssa.Store:
			if len(*instr.Addr.Referrers()) == 0 {
				return
			}

			for _, aRef := range *instr.Addr.Referrers() {
				if c, ok := aRef.(*ssa.MakeClosure); ok {
					if f, ok := c.Fn.(*ssa.Function); ok {
						for _, b := range f.Blocks {
							checkDeferred(pass, &b.Instrs, targetTypes, true)
						}
					}
				}
			}
		case *ssa.UnOp:
			instrType := instr.Type()
			for _, targetType := range targetTypes {
				var tt types.Type

				switch t := targetType.(type) {
				case *types.Pointer:
					tt = t
				case *types.Named:
					tt = t
				default:
					continue
				}

				if types.Identical(instrType, tt) {
					checkDeferred(pass, instr.Referrers(), targetTypes, inDefer)
				}
			}
		case *ssa.FieldAddr:
			checkDeferred(pass, instr.Referrers(), targetTypes, inDefer)
		}
	}
}
