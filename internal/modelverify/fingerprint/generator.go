package fingerprint

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
)

// 随机出题器。
//
// 为什么不一直重放那 36 道固定题：固定题是公开的（参考库就是用它们采的），
// 中转站只要认出这 36 段文字，就能只把"检测请求"转给真模型、平时照样掺假。
// 随机题让每一次检测的措辞、语言和数量都不一样，没法靠字面识别。
//
// 为什么随机题仍然可比：参考库的排名器本身就扣掉了"提示环境"带来的整体偏移
// （见 rank.go 的 nuisance 投影），上游 lm-detector 的网页检测也是用随机题。
// 我们只学这个做法，措辞是自己写的：任务语义保持一致——
// 逐个位置凭第一反应选 1..355 的整数、不许数数/单调/等差/循环/重复块、
// 不许用工具或代码、允许偶然重复、不回头修改、直接从第一个数开始输出。
//
// 固定 36 题保留为"对照"模式（PickChallenges），两种模式的结论应当一致；
// 不一致本身就是线索。

// ConditionRandom 标记随机生成的题目。
const ConditionRandom = "random"

const (
	generatedMinCount = 286
	generatedMaxCount = 330
)

// promptBundle 是一种语言的全部等价措辞。每道题从每一组里独立抽一句。
type promptBundle struct {
	code      string
	joiner    string
	openers   []string
	asks      []func(count int) string
	rules     []string
	tools     []string
	repeats   []string
	formats   []string
	beginners []string
}

var promptBundles = []promptBundle{
	{
		code:   "zh",
		joiner: "",
		openers: []string{
			"下面是一个没有标准答案的取数小任务。",
			"请完成一次凭直觉挑数字的记录。",
			"这是一道纯粹的数字挑选题，数字本身不代表任何含义。",
			"接下来请你随手挑一串数字。",
		},
		asks: []func(int) string{
			func(n int) string {
				return fmt.Sprintf("请一个一个地挑出 %d 个整数，每个都在 1 到 355 之间（包含 1 和 355）。", n)
			},
			func(n int) string {
				return fmt.Sprintf("一共写出 %d 个取值，范围是 1 至 355 的整数，两端都可以取到。", n)
			},
			func(n int) string {
				return fmt.Sprintf("凭第一感觉依次给出 %d 个整数，取值限定在 1～355（含两端）。", n)
			},
		},
		rules: []string{
			"每个位置都单独凭直觉决定；不要从 1 开始往上数，不要写成连续变大或连续变小的数列，也不要用等差、循环、成段重复之类的规律。",
			"每一项都要单独挑选，不要按顺序数数，不要让数字一路升高或一路降低，也不要套用等差数列、周期循环或重复片段等任何规律。",
		},
		tools: []string{
			"请你自己直接完成，不要调用任何工具，包括代码、Python、计算器、搜索、API 或随机数生成器，也不要先写程序再输出。",
			"全程由你本人直接作答：不使用任何工具（代码执行、Python、计算器、搜索、API、随机数生成器都不行），也不要先写代码。",
		},
		repeats: []string{
			"同一个数出现不止一次没关系；写下的数不要再回头排序、去重或改动。",
			"偶尔重复是正常的，已经写出来的数字保持原样，不要整理或替换。",
			"允许出现重复值，也不必删掉；不要回过头调整前面写过的数。",
		},
		formats: []string{
			"数字之间用逗号、空格或换行隔开都可以。",
			"分隔方式随意，只要每个整数之间界限清楚。",
			"用同一种常见的分隔符把数字隔开即可。",
		},
		beginners: []string{
			"直接从第一个数开始写，前面不要复述数量、范围或要求。",
			"开头就是第一个数字，不要先重复题目说明。",
		},
	},
	{
		code:   "en",
		joiner: " ",
		openers: []string{
			"Here is a small number-picking task with no right answer.",
			"Please record a run of gut-feeling number choices.",
			"This is a plain number-choosing exercise; the numbers carry no meaning.",
			"Next, jot down a string of numbers off the top of your head.",
		},
		asks: []func(int) string{
			func(n int) string {
				return fmt.Sprintf("Pick %d whole numbers one at a time, each between 1 and 355 inclusive.", n)
			},
			func(n int) string {
				return fmt.Sprintf("Write out %d values in total, each an integer from 1 to 355 (both ends allowed).", n)
			},
			func(n int) string {
				return fmt.Sprintf("Give %d integers in a row by first instinct, each within 1-355, inclusive.", n)
			},
		},
		rules: []string{
			"Decide every position on its own; do not count upward from 1, do not write runs that keep rising or keep falling, and do not follow arithmetic steps, cycles, repeated chunks, or any other rule.",
			"Choose each item separately: no counting in order, no steadily increasing or decreasing stretches, and no arithmetic sequences, periodic loops, repeated segments, or other patterns.",
		},
		tools: []string{
			"Do this yourself, directly: do not use any tool, including code, Python, a calculator, search, an API, or a random number generator, and do not write a program first.",
			"Answer entirely on your own without tools (no code execution, Python, calculator, search, API, or random number generator), and do not draft code beforehand.",
		},
		repeats: []string{
			"It is fine if a number shows up more than once; never go back to sort, deduplicate, or change what you have written.",
			"Occasional repeats are normal; leave the numbers already written exactly as they are.",
			"Repeated values are allowed and need not be removed; do not revise earlier numbers.",
		},
		formats: []string{
			"Separate the numbers with commas, spaces, or line breaks.",
			"Any separator works as long as each integer is clearly delimited.",
			"Use one ordinary separator consistently between numbers.",
		},
		beginners: []string{
			"Start right away with the first number; do not restate the count, the range, or these instructions.",
			"Begin with the first number itself, without repeating the task first.",
		},
	},
	{
		code:   "ja",
		joiner: "",
		openers: []string{
			"正解のない、数字を選ぶだけの小さな課題です。",
			"直感で数字を選んだ記録を一つ作ってください。",
			"数字そのものに意味はない、単純な数選びの課題です。",
		},
		asks: []func(int) string{
			func(n int) string {
				return fmt.Sprintf("1 から 355 まで（両端を含む）の整数を、一つずつ %d 個選んでください。", n)
			},
			func(n int) string {
				return fmt.Sprintf("1〜355（両端を含む）の整数を、直感で合計 %d 個書き出してください。", n)
			},
		},
		rules: []string{
			"各位置の値はそれぞれ別々に決めてください。1 から順に数えたり、増え続ける・減り続ける並びにしたり、等差・循環・同じかたまりの繰り返しなどの規則に従ったりしないでください。",
			"一つずつ独立に選び、順番に数える、単調に増減させる、等差数列や周期、繰り返しのパターンを使う、といったことは避けてください。",
		},
		tools: []string{
			"あなた自身が直接答えてください。コード、Python、電卓、検索、API、乱数生成器などの道具は一切使わず、先にプログラムを書くこともしないでください。",
		},
		repeats: []string{
			"同じ数が何度出てもかまいません。書いた数を後から並べ替えたり、重複を消したり、書き換えたりしないでください。",
			"たまたま重なった値もそのままで有効です。前に書いた数は直さないでください。",
		},
		formats: []string{
			"数字の区切りはカンマ、スペース、改行のどれでもかまいません。",
			"各整数の境目がはっきりしていれば、区切り方は自由です。",
		},
		beginners: []string{
			"前置きなしで、最初の数からすぐに書き始めてください。個数や範囲、指示を繰り返さないでください。",
		},
	},
	{
		code:   "fr",
		joiner: " ",
		openers: []string{
			"Voici un petit exercice de choix de nombres, sans bonne réponse.",
			"Notez une série de nombres choisis au feeling.",
			"Il s'agit simplement de choisir des nombres ; ils n'ont aucune signification.",
		},
		asks: []func(int) string{
			func(n int) string {
				return fmt.Sprintf("Choisissez %d nombres entiers un par un, chacun compris entre 1 et 355 inclus.", n)
			},
			func(n int) string {
				return fmt.Sprintf("Écrivez au total %d valeurs entières de 1 à 355, bornes comprises, selon votre première intuition.", n)
			},
		},
		rules: []string{
			"Décidez chaque position séparément : ne comptez pas à partir de 1, n'écrivez pas de suites qui montent ou descendent sans cesse, et n'utilisez ni progression arithmétique, ni cycle, ni bloc répété, ni autre règle.",
			"Choisissez chaque élément indépendamment, sans compter dans l'ordre, sans séquence croissante ou décroissante, sans progression, boucle ou motif répété.",
		},
		tools: []string{
			"Répondez vous-même, directement : n'utilisez aucun outil (code, Python, calculatrice, recherche, API ou générateur aléatoire) et n'écrivez pas de programme au préalable.",
		},
		repeats: []string{
			"Un même nombre peut revenir plusieurs fois ; ne revenez pas en arrière pour trier, dédoublonner ou modifier ce qui est écrit.",
			"Les répétitions occasionnelles sont normales ; laissez tels quels les nombres déjà écrits.",
		},
		formats: []string{
			"Séparez les nombres par des virgules, des espaces ou des retours à la ligne.",
			"N'importe quel séparateur convient tant que chaque entier est bien délimité.",
		},
		beginners: []string{
			"Commencez directement par le premier nombre, sans rappeler la quantité, l'intervalle ni les consignes.",
		},
	},
	{
		code:   "es",
		joiner: " ",
		openers: []string{
			"Esta es una pequeña tarea de elegir números, sin respuesta correcta.",
			"Anota una serie de números elegidos por intuición.",
			"Se trata solo de escoger números; no significan nada.",
		},
		asks: []func(int) string{
			func(n int) string {
				return fmt.Sprintf("Elige %d números enteros uno por uno, cada uno entre 1 y 355, ambos incluidos.", n)
			},
			func(n int) string {
				return fmt.Sprintf("Escribe en total %d valores enteros del 1 al 355 (extremos incluidos) según tu primera impresión.", n)
			},
		},
		rules: []string{
			"Decide cada posición por separado: no cuentes desde 1, no escribas tramos que suban o bajen sin parar y no sigas progresiones aritméticas, ciclos, bloques repetidos ni ninguna otra regla.",
			"Elige cada elemento de forma independiente, sin contar en orden, sin secuencias crecientes o decrecientes y sin progresiones, bucles ni patrones repetidos.",
		},
		tools: []string{
			"Hazlo tú mismo, directamente: no uses ninguna herramienta (código, Python, calculadora, búsqueda, API ni generador de números aleatorios) y no escribas un programa antes.",
		},
		repeats: []string{
			"Un número puede aparecer más de una vez; no vuelvas atrás para ordenar, quitar duplicados ni cambiar lo escrito.",
			"Las repeticiones ocasionales son normales; deja tal cual los números ya escritos.",
		},
		formats: []string{
			"Separa los números con comas, espacios o saltos de línea.",
			"Cualquier separador sirve mientras cada entero quede bien delimitado.",
		},
		beginners: []string{
			"Empieza directamente por el primer número, sin repetir la cantidad, el rango ni las instrucciones.",
		},
	},
}

// LanguageNames 是语言代码到展示名的映射（前端也会显示）。
var LanguageNames = map[string]string{
	"zh": "中文",
	"en": "English",
	"ja": "日本語",
	"fr": "Français",
	"es": "Español",
}

// randIntn 返回 [0, n) 的均匀随机数。用 crypto/rand：出题的随机性就是防识别的
// 全部意义，可预测的种子等于把题目提前告诉中转站。
func randIntn(n int) int {
	if n <= 1 {
		return 0
	}
	limit := ^uint64(0) - (^uint64(0) % uint64(n))
	var buf [8]byte
	for {
		if _, err := rand.Read(buf[:]); err != nil {
			panic(fmt.Sprintf("crypto/rand 不可用: %v", err))
		}
		v := binary.LittleEndian.Uint64(buf[:])
		if v < limit {
			return int(v % uint64(n))
		}
	}
}

// GenerateChallenges 生成 count 道随机题。每道题的数量互不相同，语言与措辞独立抽取。
func GenerateChallenges(count int) []Challenge {
	return generateChallenges(count, randIntn)
}

func generateChallenges(count int, intn func(int) int) []Challenge {
	if count < 1 {
		count = 1
	}
	pool := make([]int, 0, generatedMaxCount-generatedMinCount+1)
	for n := generatedMinCount; n <= generatedMaxCount; n++ {
		pool = append(pool, n)
	}
	out := make([]Challenge, 0, count)
	for i := 0; i < count; i++ {
		pick := intn(len(pool))
		n := pool[pick]
		pool = append(pool[:pick], pool[pick+1:]...)
		bundle := promptBundles[intn(len(promptBundles))]
		out = append(out, Challenge{
			Condition:     ConditionRandom,
			CHALLENGEID:   fmt.Sprintf("random-%s-%d", bundle.code, n),
			ExpectedCount: n,
			Prompt:        bundle.render(n, intn),
			Language:      bundle.code,
		})
	}
	return out
}

func (b promptBundle) render(n int, intn func(int) int) string {
	pick := func(list []string) string { return list[intn(len(list))] }
	parts := []string{
		pick(b.openers),
		b.asks[intn(len(b.asks))](n),
		pick(b.rules),
		pick(b.tools),
		pick(b.repeats),
		pick(b.formats),
		pick(b.beginners),
	}
	return strings.Join(parts, b.joiner)
}
