package billingexpr

import (
	"slices"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
)

// HideGroups returns the expression as it may be shown to a viewer who can
// use none of the hidden groups. None of that viewer's requests bill in a
// hidden group, so `group == "<hidden>"` is false for them: such comparisons
// are folded away together with the conditions and branches they decide, and
// hidden names are dropped from `group in [...]` lists. The result prices
// every group the viewer can use exactly as the original does.
//
// An expression that names no hidden group is returned verbatim. One that
// does is printed back from its folded syntax tree, without comments. ok is
// false when a hidden name survives folding (a tier name, a pattern, any use
// of group other than ==, != and in) or the expression declares variables;
// callers must then withhold it.
func HideGroups(exprStr string, hidden []string) (shown string, ok bool) {
	hidden = slices.DeleteFunc(slices.Clone(hidden), func(group string) bool { return group == "" })
	isHidden := func(group string) bool { return slices.Contains(hidden, group) }
	namesHidden := func(text string) bool {
		return slices.ContainsFunc(hidden, func(group string) bool { return strings.Contains(text, group) })
	}
	_, body := ParseExprVersion(exprStr)
	// Nothing to hide unless a name is in the text, spelled out or escaped.
	if !namesHidden(body) && !strings.Contains(body, `\`) {
		return exprStr, true
	}
	tree, err := parser.Parse(body)
	if err != nil {
		return "", false
	}
	folder := &hiddenGroupFolder{isHidden: isHidden}
	ast.Walk(&tree.Node, folder)

	// A literal compared whole cannot embed another name; any other string
	// (a pattern, a searched text, a header) is matched by substring.
	exact := map[ast.Node]bool{}
	ast.Find(tree.Node, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.BinaryNode:
			switch n.Operator {
			case "==", "!=":
				exact[n.Left], exact[n.Right] = true, true
			case "in":
				if list, isList := n.Right.(*ast.ArrayNode); isList {
					for _, element := range list.Nodes {
						exact[element] = true
					}
				}
			}
		case *ast.CallNode:
			callee, _ := n.Callee.(*ast.IdentifierNode)
			if callee != nil && slices.Contains([]string{"tier", "hour", "minute", "weekday", "month", "day"}, callee.Value) {
				for _, argument := range n.Arguments {
					exact[argument] = true
				}
			}
		}
		return false
	})
	leftover := ast.Find(tree.Node, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.StringNode:
			if exact[node] {
				return isHidden(n.Value)
			}
			return namesHidden(n.Value)
		case *ast.BytesNode:
			return namesHidden(string(n.Value))
		case *ast.VariableDeclaratorNode:
			// A variable can carry a name, and the printer does not
			// parenthesize a declaration used as an operand.
			return true
		}
		return false
	})
	if leftover != nil {
		return "", false
	}
	if !folder.folded {
		// The name was only in a comment, or is not a use this viewer
		// would recognize as a group; print without the comment.
		if !strings.Contains(body, "//") && !strings.Contains(body, "/*") {
			return exprStr, true
		}
	}
	printed := tree.Node.String()
	if _, err := parser.Parse(printed); err != nil {
		return "", false
	}
	return exprStr[:len(exprStr)-len(body)] + printed, true
}

// hiddenGroupFolder folds, bottom up, everything a comparison of group with a
// hidden name decides.
type hiddenGroupFolder struct {
	isHidden func(group string) bool
	folded   bool
}

func (f *hiddenGroupFolder) Visit(node *ast.Node) {
	replace := func(replacement ast.Node) {
		ast.Patch(node, replacement)
		f.folded = true
	}
	constant := func(operand ast.Node) (value, known bool) {
		literal, isBool := operand.(*ast.BoolNode)
		return isBool && literal.Value, isBool
	}
	switch n := (*node).(type) {
	case *ast.UnaryNode:
		if value, known := constant(n.Node); known && (n.Operator == "!" || n.Operator == "not") {
			replace(&ast.BoolNode{Value: !value})
		}
	case *ast.ConditionalNode:
		if value, known := constant(n.Cond); known {
			if value {
				replace(n.Exp1)
			} else {
				replace(n.Exp2)
			}
		}
	case *ast.BinaryNode:
		switch n.Operator {
		case "==", "!=":
			identifier, _ := n.Left.(*ast.IdentifierNode)
			literal, _ := n.Right.(*ast.StringNode)
			if identifier == nil || literal == nil {
				identifier, _ = n.Right.(*ast.IdentifierNode)
				literal, _ = n.Left.(*ast.StringNode)
			}
			if identifier != nil && literal != nil && identifier.Value == "group" && f.isHidden(literal.Value) {
				replace(&ast.BoolNode{Value: n.Operator == "!="})
			}
		case "in":
			identifier, _ := n.Left.(*ast.IdentifierNode)
			list, _ := n.Right.(*ast.ArrayNode)
			if identifier == nil || list == nil || identifier.Value != "group" {
				return
			}
			visible := slices.DeleteFunc(slices.Clone(list.Nodes), func(element ast.Node) bool {
				literal, isString := element.(*ast.StringNode)
				return isString && f.isHidden(literal.Value)
			})
			if len(visible) == len(list.Nodes) {
				return
			}
			if len(visible) == 0 {
				replace(&ast.BoolNode{Value: false})
				return
			}
			list.Nodes = visible
			f.folded = true
		case "&&", "and", "||", "or":
			// The absorbing value decides the result; the other one drops out.
			absorbing := n.Operator == "||" || n.Operator == "or"
			left, leftKnown := constant(n.Left)
			right, rightKnown := constant(n.Right)
			switch {
			case leftKnown && left == absorbing, rightKnown && right == absorbing:
				replace(&ast.BoolNode{Value: absorbing})
			case leftKnown:
				replace(n.Right)
			case rightKnown:
				replace(n.Left)
			}
		}
	}
}

// RequestRules lists the request rules an expression declares, in trace
// order and unevaluated. It compiles without the shared program cache:
// callers pass display variants.
func RequestRules(exprStr string) ([]RequestRuleTrace, error) {
	version, body := ParseExprVersion(exprStr)
	patcher := &requestRulePatcher{}
	if _, err := expr.Compile(body, expr.Env(getCompileEnv(version)), expr.Patch(patcher), expr.AsFloat64()); err != nil {
		return nil, err
	}
	return patcher.requestRules, nil
}
