package main

// ==================== utf8mb4_general_ci 折叠比较 ====================
// MySQL 的列默认排序规则是 utf8mb4_general_ci：等值与范围比较都忽略大小写。
// 本文件的折叠语义**严格等价 strings.EqualFold**（即 Unicode 简单折叠 simple
// case folding），而不是"只把 ASCII 大写转小写"——因为 Go 的 EqualFold 会把
// 'K'(U+212A) 与 'k'、'ſ'(U+017F) 与 's' 视为相等，只做 ToLower 会让比较与
// 索引键编码出现分歧（比较说相等、索引键不同 → 走索引查不到）。
//
// 做法：把每个 rune 映射到其 SimpleFold 轨道里的最小 rune。同一轨道的所有
// rune 折叠结果相同，因此：
//
//	e1 == e2（EqualFold）  ⇔  foldRune(e1) == foldRune(e2)  ⇔  foldKey(s1) == foldKey(s2)
//
// 这给出一个确定的全序（按折叠后 rune 的码点），因此等值、范围、索引键、
// ORDER BY 可以共用同一套语义：
//   - 比较：cmpFold / eqFold（不分配）；
//   - 索引键：foldKey（把折叠结果写进索引树，范围界也用同一函数折叠）。
//
// 已知差异（相对 MySQL general_ci）：不做重音折叠（'é' ≠ 'e'）、不做
// "ß" ⇄ "ss" 展开；这两点与 strings.EqualFold 保持一致。

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// foldRune 返回 r 在 Unicode 简单折叠轨道中的最小 rune。
// SimpleFold 的轨道是一个闭环，因此循环一定终止；无折叠对（如数字/标点）
// 的轨道就是自身，直接返回 r。
func foldRune(r rune) rune {
	min := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < min {
			min = f
		}
	}
	return min
}

// foldKey 返回字符串的折叠规范形，用作索引键 / 哈希键。
// 纯 ASCII 已小写的字符串零拷贝直接返回（绝大多数业务数据走这条路径）。
func foldKey(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		need := false
		for i := 0; i < len(s); i++ {
			if s[i] >= 'A' && s[i] <= 'Z' {
				need = true
				break
			}
		}
		if !need {
			return s
		}
		b := []byte(s)
		for i := range b {
			if b[i] >= 'A' && b[i] <= 'Z' {
				b[i] += 'a' - 'A'
			}
		}
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(s))
	changed := false
	for _, r := range s {
		f := foldRune(r)
		if f != r {
			changed = true
		}
		sb.WriteRune(f)
	}
	if !changed {
		return s
	}
	return sb.String()
}

// cmpFold 按折叠语义比较两个字符串，返回 -1 / 0 / 1。
// 与 strings.EqualFold 一致：非法 UTF-8 字节按 RuneError 处理。
// 零分配：逐个 rune 折叠后比较，相等才按原始字节宽度前进。
func cmpFold(a, b string) int {
	for len(a) > 0 && len(b) > 0 {
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		ra, rb = foldRune(ra), foldRune(rb)
		if ra != rb {
			if ra < rb {
				return -1
			}
			return 1
		}
		a, b = a[na:], b[nb:]
	}
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return -1
	default:
		return 1
	}
}

// eqFold 等值比较。直接复用标准库，语义与 foldKey/cmpFold 一致。
func eqFold(a, b string) bool { return strings.EqualFold(a, b) }
