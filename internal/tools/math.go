package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// MathTool évalue une expression mathématique (arithmétique, puissances,
// fonctions usuelles, constantes) en flottants 64 bits. Pur calcul local :
// aucun accès fichier/réseau/processus, donc ni permission ni sandbox. Évite
// que le modèle fasse de l'arithmétique "de tête", source d'erreurs.
type MathTool struct{}

func (t *MathTool) Name() string { return "math" }

func (t *MathTool) Description() string {
	return "Évalue une expression mathématique et retourne le résultat exact du calcul (à préférer à un calcul de tête dès que ce n'est pas trivial). " +
		"Opérateurs : + - * / % (modulo) ^ ou ** (puissance), parenthèses. " +
		"Fonctions : sqrt, cbrt, abs, floor, ceil, round, trunc, exp, ln, log (base 10), log2, sin, cos, tan, asin, acos, atan, sinh, cosh, tanh, sign, deg (radians→degrés), rad (degrés→radians), pow(x,y), min(a,b,...), max(a,b,...), hypot(x,y), atan2(y,x), mod(x,y), round(x,n) (n décimales). " +
		"Constantes : pi, e, tau. Les angles des fonctions trigonométriques sont en radians. Exemple : \"sqrt(2) * (3 + 4)^2 / 7\"."
}

func (t *MathTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"expression": {"type": "string", "description": "Expression à évaluer, ex: \"(12.5 * 3) / 4 + sqrt(16)\". Séparateur décimal : le point."}
		},
		"required": ["expression"],
		"additionalProperties": false
	}`)
}

type mathArgs struct {
	Expression string `json:"expression"`
}

func (t *MathTool) Call(ctx context.Context, argsJSON string) (string, error) {
	var args mathArgs
	if err := decodeArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Expression) == "" {
		return "", fmt.Errorf(`paramètre "expression" requis`)
	}
	v, err := evalMath(args.Expression)
	if err != nil {
		return "", err
	}
	return formatMathResult(v), nil
}

func formatMathResult(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// evalMath évalue expr ; une division par zéro ou un résultat hors domaine
// (NaN, ±Inf) est une erreur explicite.
func evalMath(expr string) (float64, error) {
	p := &mathParser{src: expr}
	v, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	p.skipSpaces()
	if p.pos < len(p.src) {
		return 0, fmt.Errorf("caractère inattendu %q à la position %d", p.src[p.pos], p.pos+1)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("résultat non défini ou infini (hors domaine)")
	}
	return v, nil
}

type mathParser struct {
	src string
	pos int
}

func (p *mathParser) skipSpaces() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t' || p.src[p.pos] == '\n') {
		p.pos++
	}
}

// peek retourne le prochain caractère non blanc (0 en fin de chaîne).
func (p *mathParser) peek() byte {
	p.skipSpaces()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *mathParser) parseExpr() (float64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		switch p.peek() {
		case '+':
			p.pos++
			r, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			left += r
		case '-':
			p.pos++
			r, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			left -= r
		default:
			return left, nil
		}
	}
}

func (p *mathParser) parseTerm() (float64, error) {
	left, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	for {
		c := p.peek()
		// "**" est la puissance, pas une multiplication.
		if c == '*' && !strings.HasPrefix(p.src[p.pos:], "**") {
			p.pos++
			r, err := p.parseUnary()
			if err != nil {
				return 0, err
			}
			left *= r
		} else if c == '/' {
			p.pos++
			r, err := p.parseUnary()
			if err != nil {
				return 0, err
			}
			if r == 0 {
				return 0, fmt.Errorf("division par zéro")
			}
			left /= r
		} else if c == '%' {
			p.pos++
			r, err := p.parseUnary()
			if err != nil {
				return 0, err
			}
			if r == 0 {
				return 0, fmt.Errorf("modulo par zéro")
			}
			left = math.Mod(left, r)
		} else {
			return left, nil
		}
	}
}

// parseUnary gère le signe, avec une priorité inférieure à la puissance :
// -2^2 = -4.
func (p *mathParser) parseUnary() (float64, error) {
	switch p.peek() {
	case '-':
		p.pos++
		v, err := p.parseUnary()
		return -v, err
	case '+':
		p.pos++
		return p.parseUnary()
	}
	return p.parsePower()
}

// parsePower est associatif à droite : 2^3^2 = 2^(3^2).
func (p *mathParser) parsePower() (float64, error) {
	base, err := p.parsePrimary()
	if err != nil {
		return 0, err
	}
	if p.peek() == '^' {
		p.pos++
	} else if strings.HasPrefix(p.src[p.pos:], "**") {
		p.pos += 2
	} else {
		return base, nil
	}
	exp, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	return math.Pow(base, exp), nil
}

func (p *mathParser) parsePrimary() (float64, error) {
	c := p.peek()
	switch {
	case c == 0:
		return 0, fmt.Errorf("expression incomplète : opérande attendu à la position %d", p.pos+1)
	case c == '(':
		p.pos++
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		if p.peek() != ')' {
			return 0, fmt.Errorf("parenthèse fermante ')' attendue à la position %d", p.pos+1)
		}
		p.pos++
		return v, nil
	case c >= '0' && c <= '9' || c == '.':
		return p.parseNumber()
	case isMathIdentStart(c):
		return p.parseIdent()
	}
	return 0, fmt.Errorf("caractère inattendu %q à la position %d", c, p.pos+1)
}

func (p *mathParser) parseNumber() (float64, error) {
	start := p.pos
	for p.pos < len(p.src) && (p.src[p.pos] >= '0' && p.src[p.pos] <= '9' || p.src[p.pos] == '.' || p.src[p.pos] == '_') {
		p.pos++
	}
	// Notation scientifique : 1e6, 2.5E-3.
	if p.pos < len(p.src) && (p.src[p.pos] == 'e' || p.src[p.pos] == 'E') {
		q := p.pos + 1
		if q < len(p.src) && (p.src[q] == '+' || p.src[q] == '-') {
			q++
		}
		if q < len(p.src) && p.src[q] >= '0' && p.src[q] <= '9' {
			for q < len(p.src) && p.src[q] >= '0' && p.src[q] <= '9' {
				q++
			}
			p.pos = q
		}
	}
	text := strings.ReplaceAll(p.src[start:p.pos], "_", "")
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("nombre invalide %q à la position %d", text, start+1)
	}
	return v, nil
}

func isMathIdentStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func (p *mathParser) parseIdent() (float64, error) {
	start := p.pos
	for p.pos < len(p.src) && (isMathIdentStart(p.src[p.pos]) || p.src[p.pos] >= '0' && p.src[p.pos] <= '9') {
		p.pos++
	}
	name := strings.ToLower(p.src[start:p.pos])

	if p.peek() != '(' {
		switch name {
		case "pi":
			return math.Pi, nil
		case "e":
			return math.E, nil
		case "tau":
			return 2 * math.Pi, nil
		}
		return 0, fmt.Errorf("constante inconnue %q (connues : pi, e, tau)", p.src[start:p.pos])
	}
	p.pos++ // '('

	var args []float64
	if p.peek() == ')' {
		p.pos++
	} else {
		for {
			v, err := p.parseExpr()
			if err != nil {
				return 0, err
			}
			args = append(args, v)
			c := p.peek()
			p.pos++
			if c == ')' {
				break
			}
			if c != ',' {
				return 0, fmt.Errorf("',' ou ')' attendu dans l'appel de %s à la position %d", name, p.pos)
			}
		}
	}
	return callMathFunc(name, args)
}

var mathFuncs1 = map[string]func(float64) float64{
	"sqrt":  math.Sqrt,
	"cbrt":  math.Cbrt,
	"abs":   math.Abs,
	"floor": math.Floor,
	"ceil":  math.Ceil,
	"trunc": math.Trunc,
	"exp":   math.Exp,
	"ln":    math.Log,
	"log":   math.Log10,
	"log10": math.Log10,
	"log2":  math.Log2,
	"sin":   math.Sin,
	"cos":   math.Cos,
	"tan":   math.Tan,
	"asin":  math.Asin,
	"acos":  math.Acos,
	"atan":  math.Atan,
	"sinh":  math.Sinh,
	"cosh":  math.Cosh,
	"tanh":  math.Tanh,
	"deg":   func(x float64) float64 { return x * 180 / math.Pi },
	"rad":   func(x float64) float64 { return x * math.Pi / 180 },
	"sign": func(x float64) float64 {
		switch {
		case x > 0:
			return 1
		case x < 0:
			return -1
		}
		return 0
	},
}

var mathFuncs2 = map[string]func(a, b float64) float64{
	"pow":   math.Pow,
	"hypot": math.Hypot,
	"atan2": math.Atan2,
	"mod":   math.Mod,
}

func callMathFunc(name string, args []float64) (float64, error) {
	if name == "round" {
		switch len(args) {
		case 1:
			return math.Round(args[0]), nil
		case 2:
			f := math.Pow(10, math.Trunc(args[1]))
			return math.Round(args[0]*f) / f, nil
		}
		return 0, fmt.Errorf("round attend 1 ou 2 arguments, %d reçu(s)", len(args))
	}
	if name == "min" || name == "max" {
		if len(args) == 0 {
			return 0, fmt.Errorf("%s attend au moins 1 argument", name)
		}
		r := args[0]
		for _, a := range args[1:] {
			if name == "min" {
				r = math.Min(r, a)
			} else {
				r = math.Max(r, a)
			}
		}
		return r, nil
	}
	if f, ok := mathFuncs1[name]; ok {
		if len(args) != 1 {
			return 0, fmt.Errorf("%s attend 1 argument, %d reçu(s)", name, len(args))
		}
		return f(args[0]), nil
	}
	if f, ok := mathFuncs2[name]; ok {
		if len(args) != 2 {
			return 0, fmt.Errorf("%s attend 2 arguments, %d reçu(s)", name, len(args))
		}
		if name == "mod" && args[1] == 0 {
			return 0, fmt.Errorf("modulo par zéro")
		}
		return f(args[0], args[1]), nil
	}
	return 0, fmt.Errorf("fonction inconnue %q", name)
}
