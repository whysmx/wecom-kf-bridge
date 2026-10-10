package bridge

import "testing"

func TestStripMarkdown(t *testing.T) {
	cases := map[string]string{
		"# 标题\n\n正文":                     "标题\n\n正文",
		"**粗体** 和 *斜体* 和 __下划__":         "粗体 和 斜体 和 下划",
		"见 [文档](https://e.com/a)":        "见 文档 (https://e.com/a)",
		"[https://e.com](https://e.com)": "https://e.com",
		"![图](https://e.com/i.png)":      "图 (https://e.com/i.png)",
		"```go\nfmt.Println(1)\n```":     "fmt.Println(1)",
		"用 `go test` 运行":                 "用 go test 运行",
		"> 引用\n* 一\n+ 二\n- 三":            "引用\n- 一\n- 二\n- 三",
		"~~删除~~\n\n\n\n---\n结尾":          "删除\n\n结尾",
		"2*3*4 保持":                       "2*3*4 保持",
		"a_b_c 和 snake_case":             "a_b_c 和 snake_case",
		"<script>alert(1)</script> 原样文本": "<script>alert(1)</script> 原样文本",
	}
	for in, want := range cases {
		if got := StripMarkdown(in); got != want {
			t.Errorf("StripMarkdown(%q)=%q want %q", in, got, want)
		}
	}
}
