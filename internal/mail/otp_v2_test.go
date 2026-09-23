package mail

import "testing"

func TestExtractOTPBoundariesKeywordsAndReadableHTML(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name                string
		subject, text, html string
		want                string
	}{
		{name: "six digits", subject: "Code: 123456", want: "123456"},
		{name: "hyphenated subject code", subject: "Your Grok verification code: 610-313", want: "610-313"},
		{name: "bold hyphenated text code", text: "Your verification code is **610-313**", want: "610-313"},
		{name: "hyphenated code with leading zeroes", text: "Code: 010-003", want: "010-003"},
		{name: "hyphenated HTML code", html: `<p>Verification code: <strong>610&#45;313</strong></p>`, want: "610-313"},
		{name: "hyphenated keyword candidate wins", subject: "Reference 123456", text: "Verification code: 610-313", want: "610-313"},
		{name: "plain keyword candidate wins", subject: "Reference 610-313", text: "Verification code: 123456", want: "123456"},
		{name: "hyphenated subject wins equal score", subject: "610-313", text: "123456", want: "610-313"},
		{name: "hyphenated short first group rejected", text: "Code: 61-313"},
		{name: "hyphenated short second group rejected", text: "Code: 610-31"},
		{name: "hyphenated long first group rejected", text: "Code: 1610-313"},
		{name: "hyphenated long second group rejected", text: "Code: 610-3131"},
		{name: "hyphenated uneven groups rejected", text: "Code: 61-0313"},
		{name: "hyphenated ASCII letter prefix rejected", text: "Code: A610-313"},
		{name: "hyphenated ASCII letter suffix rejected", text: "Code: 610-313Z"},
		{name: "hyphenated Unicode letter adjacency rejected", text: "Code: 验610-313证"},
		{name: "hyphenated Unicode number adjacency rejected", text: "Code: Ⅷ610-313"},
		{name: "hyphenated full-width digits rejected", text: "Code: ６１０-３１３"},
		{name: "hyphenated extra leading group rejected", text: "Phone: 1-610-313"},
		{name: "hyphenated extra trailing group rejected", text: "Phone: 610-313-1234"},
		{name: "hyphenated HTML hidden text ignored", html: `<style>code 111-222</style><script>code 333-444</script><p>Code: <b>610-313</b></p>`, want: "610-313"},
		{name: "three digits rejected", subject: "Code 123"},
		{name: "four-digit year rejected", subject: "账单年份 2026"},
		{name: "five digits rejected", subject: "Code 12345"},
		{name: "seven digits rejected", subject: "Code 1234567"},
		{name: "eight digits rejected", text: "验证码 12345678"},
		{name: "nine digits rejected", subject: "Code 123456789"},
		{name: "ASCII letter adjacency rejected", subject: "code A123456Z"},
		{name: "ASCII digit adjacency rejected", subject: "code 912345678"},
		{name: "Unicode letter adjacency rejected", subject: "code 验123456证"},
		{name: "Unicode number adjacency rejected", subject: "code Ⅷ123456"},
		{
			name:    "keyword candidate wins",
			subject: "Order 20260811; 验证码 654321",
			want:    "654321",
		},
		{
			name:    "four-digit year ignored before code",
			subject: "Date 2026; verification code 135790",
			want:    "135790",
		},
		{
			name: "nearest keyword wins",
			text: "reference 111111 then a long description; verification code 222222",
			want: "222222",
		},
		{
			name:    "subject wins equal score",
			subject: "123456",
			text:    "567890",
			want:    "123456",
		},
		{
			name: "HTML readable text and ignored script",
			html: `<html><head><title>code 111111</title></head><body>` +
				`<script>verification code 999999</script><p>Verification code <b>246810</b></p></body></html>`,
			want: "246810",
		},
		{name: "full-width digits are not ASCII OTP", text: "验证码 １２３４５６"},
		{name: "no code", subject: "Welcome", text: "No numeric token here"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractOTP(test.subject, test.text, test.html); got != test.want {
				t.Fatalf("ExtractOTP() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestArchiveMessageHardLimitIncludesExactlyOneHundredMiB(t *testing.T) {
	t.Parallel()

	settings := NewFetcher().settings()
	if settings.maxMessageBytes != 100<<20 {
		t.Fatalf("default archive message limit = %d, want %d", settings.maxMessageBytes, 100<<20)
	}
	if messageExceedsArchiveLimit(100<<20, settings.maxMessageBytes) {
		t.Fatal("exactly 100 MiB was classified as oversized")
	}
	if !messageExceedsArchiveLimit(100<<20+1, settings.maxMessageBytes) {
		t.Fatal("100 MiB + 1 byte was not classified as oversized")
	}
}
