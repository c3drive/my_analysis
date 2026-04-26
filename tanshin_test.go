package main

import "testing"

func TestParseJPNumber(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1,234", 1234},
		{"1234", 1234},
		{"-100", -100},
		{"△500", -500},
		{"▲1,000", -1000},
		{"  500  ", 500},
		{"abc", 0},      // パース不可は 0
		{"", 0},
	}
	for _, c := range cases {
		got := parseJPNumber(c.in)
		if got != c.want {
			t.Errorf("parseJPNumber(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseTanshinText_Annual_NormalFormat(t *testing.T) {
	// 標準的な決算短信のテキストレイアウト (簡易版)
	text := `
                              (百万円)
1．連結業績
                                        当期      前期
売上高              123,456     100,000
営業利益             15,000      12,000
経常利益             14,500      11,500
親会社株主に帰属する当期純利益    9,000        7,000
総資産              500,000     480,000
純資産              200,000     180,000
`
	d := parseTanshinText(text)

	// 単位は百万円なので 123,456 * 1_000_000 = 1,234.56 億
	if d.NetSales != 123_456_000_000 {
		t.Errorf("NetSales = %d, want 123,456,000,000", d.NetSales)
	}
	if d.OperatingIncome != 15_000_000_000 {
		t.Errorf("OperatingIncome = %d, want 15,000,000,000", d.OperatingIncome)
	}
	if d.NetIncome != 9_000_000_000 {
		t.Errorf("NetIncome = %d, want 9,000,000,000", d.NetIncome)
	}
	if d.TotalAssets != 500_000_000_000 {
		t.Errorf("TotalAssets = %d, want 500,000,000,000", d.TotalAssets)
	}
	if d.NetAssets != 200_000_000_000 {
		t.Errorf("NetAssets = %d, want 200,000,000,000", d.NetAssets)
	}
}

func TestParseTanshinText_NegativeIncome(t *testing.T) {
	// 純利益が赤字 (△表記)
	text := `
(百万円)
売上高              50,000   45,000
営業利益              1,000    △500
親会社株主に帰属する当期純利益   △2,000   △1,000
`
	d := parseTanshinText(text)
	if d.OperatingIncome != 1_000_000_000 {
		t.Errorf("OperatingIncome = %d, want 1,000,000,000", d.OperatingIncome)
	}
	if d.NetIncome != -2_000_000_000 {
		t.Errorf("NetIncome (loss) = %d, want -2,000,000,000", d.NetIncome)
	}
}

func TestParseTanshinText_ValidationRejectsCrazyValues(t *testing.T) {
	// 売上が異常 (50兆超 = PDF表崩れの典型) → 全リセット
	text := `
(百万円)
売上高    99,999,999,999   500,000
営業利益       1,000      900
親会社株主に帰属する当期純利益    500      400
総資産    1,000,000,000  900,000,000
純資産       500,000      450,000
`
	d := parseTanshinText(text)
	// 売上 99.99兆 (50兆超) なので全リセット
	if d.NetSales != 0 || d.NetIncome != 0 {
		t.Errorf("expected all-zero for crazy NetSales, got NetSales=%d NetIncome=%d", d.NetSales, d.NetIncome)
	}
}

func TestParseTanshinText_NegativeSalesRejected(t *testing.T) {
	// 売上がマイナス (パーセンテージ誤読の典型) → 全リセット
	text := `
(百万円)
売上高          -415        500
営業利益       1,000        900
親会社株主に帰属する当期純利益    616        400
`
	d := parseTanshinText(text)
	if d.NetSales != 0 || d.OperatingIncome != 0 || d.NetIncome != 0 {
		t.Errorf("expected all-zero for negative NetSales, got %+v", d)
	}
}

func TestParseTanshinText_NetIncomeValidationDropsHugePureProfit(t *testing.T) {
	// 純利益が売上の3倍超 (純資産値の混入) → 純利益のみ 0
	text := `
(百万円)
売上高              10,000     9,000
親会社株主に帰属する当期純利益   100,000    90,000
`
	d := parseTanshinText(text)
	if d.NetSales != 10_000_000_000 {
		t.Errorf("NetSales should remain, got %d", d.NetSales)
	}
	if d.NetIncome != 0 {
		t.Errorf("crazy NetIncome should be reset to 0, got %d", d.NetIncome)
	}
}
