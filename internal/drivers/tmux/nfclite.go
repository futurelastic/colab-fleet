package tmux

import (
	"strings"
	"unicode"
)

// This file is generated-by-hand (colab-fleet: terminal path v2, item 2b).
//
// # Why this exists instead of an import
//
// composer-region landed confirmation (terminalpath2.go) has to compare the
// text this driver sent against the text the composer echoes back, and the
// two can differ in COMPOSING FORM alone: the same visible character "e with
// an acute accent" is one code point in NFC (U+00E9) and two in NFD (U+0065
// U+0301), and which one a caller's or a runtime's own text-input pipeline
// produces is not something this driver controls. A byte comparison that
// does not know this treats identical text as different text.
//
// The correct general answer is Unicode's own NFC algorithm, which lives in
// golang.org/x/text/unicode/norm — not the standard library, and this
// repository's own convention (see the top-level CLAUDE.md router table:
// "colab-fleet ... Go, no deps") is to take none. This file is the
// alternative that convention leaves open: a table generated MECHANICALLY
// (not typed from memory — see the generator comment below) from Go's own
// stdlib unicode.Decompose-equivalent knowledge, covering exactly the
// characters colab-fleet's own round-1 research measured in the fixtures
// this change answers (uni-vi-nfc-*/uni-vi-nfd-* — Vietnamese text sent
// through the terminal path), plus the general Latin-1/Latin Extended-A
// diacritics that share the same combining marks.
//
// # What this is NOT
//
//   - NOT full Unicode NFC. It composes exactly the base-Latin-letter +
//     combining-mark sequences in nfcLiteTable below, canonical-ORDER
//     sequences only (see composeNFCLite's own doc comment for what
//     "canonical order" is assumed here and why).
//   - NOT a replacement for golang.org/x/text/unicode/norm in any context
//     that needs correctness beyond Latin scripts, or beyond composing
//     forms that already arrive in canonical combining-class order.
//
// Regenerate this table (Python, stdlib `unicodedata` only, itself an
// authoritative Unicode database rather than a hand-typed guess — this is
// what makes 488 entries trustworthy without 488 lines of manual review):
//
//	python3 - <<'PY'
//	import unicodedata as ud
//	for cp in range(0x00C0, 0x1EFA):
//	    ch = chr(cp)
//	    nfd = ud.normalize("NFD", ch)
//	    if len(nfd) < 2 or ud.normalize("NFC", nfd) != ch:
//	        continue
//	    if not nfd[0].isascii() or not nfd[0].isalpha():
//	        continue
//	    print(nfd, "->", ch)
//	PY
//
// nfcLiteTable maps a canonical-order decomposed sequence (base Latin letter
// plus one or two combining marks, as a Go string of the literal runes) to
// the single precomposed rune it stands for.
var nfcLiteTable = map[string]rune{
	"\u0041\u0300":       0x00C0,
	"\u0041\u0301":       0x00C1,
	"\u0041\u0302":       0x00C2,
	"\u0041\u0303":       0x00C3,
	"\u0041\u0304":       0x0100,
	"\u0041\u0306":       0x0102,
	"\u0041\u0307":       0x0226,
	"\u0041\u0308":       0x00C4,
	"\u0041\u0309":       0x1EA2,
	"\u0041\u030a":       0x00C5,
	"\u0041\u030c":       0x01CD,
	"\u0041\u030f":       0x0200,
	"\u0041\u0311":       0x0202,
	"\u0041\u0323":       0x1EA0,
	"\u0041\u0325":       0x1E00,
	"\u0041\u0328":       0x0104,
	"\u0042\u0307":       0x1E02,
	"\u0042\u0323":       0x1E04,
	"\u0042\u0331":       0x1E06,
	"\u0043\u0301":       0x0106,
	"\u0043\u0302":       0x0108,
	"\u0043\u0307":       0x010A,
	"\u0043\u030c":       0x010C,
	"\u0043\u0327":       0x00C7,
	"\u0044\u0307":       0x1E0A,
	"\u0044\u030c":       0x010E,
	"\u0044\u0323":       0x1E0C,
	"\u0044\u0327":       0x1E10,
	"\u0044\u032d":       0x1E12,
	"\u0044\u0331":       0x1E0E,
	"\u0045\u0300":       0x00C8,
	"\u0045\u0301":       0x00C9,
	"\u0045\u0302":       0x00CA,
	"\u0045\u0303":       0x1EBC,
	"\u0045\u0304":       0x0112,
	"\u0045\u0306":       0x0114,
	"\u0045\u0307":       0x0116,
	"\u0045\u0308":       0x00CB,
	"\u0045\u0309":       0x1EBA,
	"\u0045\u030c":       0x011A,
	"\u0045\u030f":       0x0204,
	"\u0045\u0311":       0x0206,
	"\u0045\u0323":       0x1EB8,
	"\u0045\u0327":       0x0228,
	"\u0045\u0328":       0x0118,
	"\u0045\u032d":       0x1E18,
	"\u0045\u0330":       0x1E1A,
	"\u0046\u0307":       0x1E1E,
	"\u0047\u0301":       0x01F4,
	"\u0047\u0302":       0x011C,
	"\u0047\u0304":       0x1E20,
	"\u0047\u0306":       0x011E,
	"\u0047\u0307":       0x0120,
	"\u0047\u030c":       0x01E6,
	"\u0047\u0327":       0x0122,
	"\u0048\u0302":       0x0124,
	"\u0048\u0307":       0x1E22,
	"\u0048\u0308":       0x1E26,
	"\u0048\u030c":       0x021E,
	"\u0048\u0323":       0x1E24,
	"\u0048\u0327":       0x1E28,
	"\u0048\u032e":       0x1E2A,
	"\u0049\u0300":       0x00CC,
	"\u0049\u0301":       0x00CD,
	"\u0049\u0302":       0x00CE,
	"\u0049\u0303":       0x0128,
	"\u0049\u0304":       0x012A,
	"\u0049\u0306":       0x012C,
	"\u0049\u0307":       0x0130,
	"\u0049\u0308":       0x00CF,
	"\u0049\u0309":       0x1EC8,
	"\u0049\u030c":       0x01CF,
	"\u0049\u030f":       0x0208,
	"\u0049\u0311":       0x020A,
	"\u0049\u0323":       0x1ECA,
	"\u0049\u0328":       0x012E,
	"\u0049\u0330":       0x1E2C,
	"\u004a\u0302":       0x0134,
	"\u004b\u0301":       0x1E30,
	"\u004b\u030c":       0x01E8,
	"\u004b\u0323":       0x1E32,
	"\u004b\u0327":       0x0136,
	"\u004b\u0331":       0x1E34,
	"\u004c\u0301":       0x0139,
	"\u004c\u030c":       0x013D,
	"\u004c\u0323":       0x1E36,
	"\u004c\u0327":       0x013B,
	"\u004c\u032d":       0x1E3C,
	"\u004c\u0331":       0x1E3A,
	"\u004d\u0301":       0x1E3E,
	"\u004d\u0307":       0x1E40,
	"\u004d\u0323":       0x1E42,
	"\u004e\u0300":       0x01F8,
	"\u004e\u0301":       0x0143,
	"\u004e\u0303":       0x00D1,
	"\u004e\u0307":       0x1E44,
	"\u004e\u030c":       0x0147,
	"\u004e\u0323":       0x1E46,
	"\u004e\u0327":       0x0145,
	"\u004e\u032d":       0x1E4A,
	"\u004e\u0331":       0x1E48,
	"\u004f\u0300":       0x00D2,
	"\u004f\u0301":       0x00D3,
	"\u004f\u0302":       0x00D4,
	"\u004f\u0303":       0x00D5,
	"\u004f\u0304":       0x014C,
	"\u004f\u0306":       0x014E,
	"\u004f\u0307":       0x022E,
	"\u004f\u0308":       0x00D6,
	"\u004f\u0309":       0x1ECE,
	"\u004f\u030b":       0x0150,
	"\u004f\u030c":       0x01D1,
	"\u004f\u030f":       0x020C,
	"\u004f\u0311":       0x020E,
	"\u004f\u031b":       0x01A0,
	"\u004f\u0323":       0x1ECC,
	"\u004f\u0328":       0x01EA,
	"\u0050\u0301":       0x1E54,
	"\u0050\u0307":       0x1E56,
	"\u0052\u0301":       0x0154,
	"\u0052\u0307":       0x1E58,
	"\u0052\u030c":       0x0158,
	"\u0052\u030f":       0x0210,
	"\u0052\u0311":       0x0212,
	"\u0052\u0323":       0x1E5A,
	"\u0052\u0327":       0x0156,
	"\u0052\u0331":       0x1E5E,
	"\u0053\u0301":       0x015A,
	"\u0053\u0302":       0x015C,
	"\u0053\u0307":       0x1E60,
	"\u0053\u030c":       0x0160,
	"\u0053\u0323":       0x1E62,
	"\u0053\u0326":       0x0218,
	"\u0053\u0327":       0x015E,
	"\u0054\u0307":       0x1E6A,
	"\u0054\u030c":       0x0164,
	"\u0054\u0323":       0x1E6C,
	"\u0054\u0326":       0x021A,
	"\u0054\u0327":       0x0162,
	"\u0054\u032d":       0x1E70,
	"\u0054\u0331":       0x1E6E,
	"\u0055\u0300":       0x00D9,
	"\u0055\u0301":       0x00DA,
	"\u0055\u0302":       0x00DB,
	"\u0055\u0303":       0x0168,
	"\u0055\u0304":       0x016A,
	"\u0055\u0306":       0x016C,
	"\u0055\u0308":       0x00DC,
	"\u0055\u0309":       0x1EE6,
	"\u0055\u030a":       0x016E,
	"\u0055\u030b":       0x0170,
	"\u0055\u030c":       0x01D3,
	"\u0055\u030f":       0x0214,
	"\u0055\u0311":       0x0216,
	"\u0055\u031b":       0x01AF,
	"\u0055\u0323":       0x1EE4,
	"\u0055\u0324":       0x1E72,
	"\u0055\u0328":       0x0172,
	"\u0055\u032d":       0x1E76,
	"\u0055\u0330":       0x1E74,
	"\u0056\u0303":       0x1E7C,
	"\u0056\u0323":       0x1E7E,
	"\u0057\u0300":       0x1E80,
	"\u0057\u0301":       0x1E82,
	"\u0057\u0302":       0x0174,
	"\u0057\u0307":       0x1E86,
	"\u0057\u0308":       0x1E84,
	"\u0057\u0323":       0x1E88,
	"\u0058\u0307":       0x1E8A,
	"\u0058\u0308":       0x1E8C,
	"\u0059\u0300":       0x1EF2,
	"\u0059\u0301":       0x00DD,
	"\u0059\u0302":       0x0176,
	"\u0059\u0303":       0x1EF8,
	"\u0059\u0304":       0x0232,
	"\u0059\u0307":       0x1E8E,
	"\u0059\u0308":       0x0178,
	"\u0059\u0309":       0x1EF6,
	"\u0059\u0323":       0x1EF4,
	"\u005a\u0301":       0x0179,
	"\u005a\u0302":       0x1E90,
	"\u005a\u0307":       0x017B,
	"\u005a\u030c":       0x017D,
	"\u005a\u0323":       0x1E92,
	"\u005a\u0331":       0x1E94,
	"\u0061\u0300":       0x00E0,
	"\u0061\u0301":       0x00E1,
	"\u0061\u0302":       0x00E2,
	"\u0061\u0303":       0x00E3,
	"\u0061\u0304":       0x0101,
	"\u0061\u0306":       0x0103,
	"\u0061\u0307":       0x0227,
	"\u0061\u0308":       0x00E4,
	"\u0061\u0309":       0x1EA3,
	"\u0061\u030a":       0x00E5,
	"\u0061\u030c":       0x01CE,
	"\u0061\u030f":       0x0201,
	"\u0061\u0311":       0x0203,
	"\u0061\u0323":       0x1EA1,
	"\u0061\u0325":       0x1E01,
	"\u0061\u0328":       0x0105,
	"\u0062\u0307":       0x1E03,
	"\u0062\u0323":       0x1E05,
	"\u0062\u0331":       0x1E07,
	"\u0063\u0301":       0x0107,
	"\u0063\u0302":       0x0109,
	"\u0063\u0307":       0x010B,
	"\u0063\u030c":       0x010D,
	"\u0063\u0327":       0x00E7,
	"\u0064\u0307":       0x1E0B,
	"\u0064\u030c":       0x010F,
	"\u0064\u0323":       0x1E0D,
	"\u0064\u0327":       0x1E11,
	"\u0064\u032d":       0x1E13,
	"\u0064\u0331":       0x1E0F,
	"\u0065\u0300":       0x00E8,
	"\u0065\u0301":       0x00E9,
	"\u0065\u0302":       0x00EA,
	"\u0065\u0303":       0x1EBD,
	"\u0065\u0304":       0x0113,
	"\u0065\u0306":       0x0115,
	"\u0065\u0307":       0x0117,
	"\u0065\u0308":       0x00EB,
	"\u0065\u0309":       0x1EBB,
	"\u0065\u030c":       0x011B,
	"\u0065\u030f":       0x0205,
	"\u0065\u0311":       0x0207,
	"\u0065\u0323":       0x1EB9,
	"\u0065\u0327":       0x0229,
	"\u0065\u0328":       0x0119,
	"\u0065\u032d":       0x1E19,
	"\u0065\u0330":       0x1E1B,
	"\u0066\u0307":       0x1E1F,
	"\u0067\u0301":       0x01F5,
	"\u0067\u0302":       0x011D,
	"\u0067\u0304":       0x1E21,
	"\u0067\u0306":       0x011F,
	"\u0067\u0307":       0x0121,
	"\u0067\u030c":       0x01E7,
	"\u0067\u0327":       0x0123,
	"\u0068\u0302":       0x0125,
	"\u0068\u0307":       0x1E23,
	"\u0068\u0308":       0x1E27,
	"\u0068\u030c":       0x021F,
	"\u0068\u0323":       0x1E25,
	"\u0068\u0327":       0x1E29,
	"\u0068\u032e":       0x1E2B,
	"\u0068\u0331":       0x1E96,
	"\u0069\u0300":       0x00EC,
	"\u0069\u0301":       0x00ED,
	"\u0069\u0302":       0x00EE,
	"\u0069\u0303":       0x0129,
	"\u0069\u0304":       0x012B,
	"\u0069\u0306":       0x012D,
	"\u0069\u0308":       0x00EF,
	"\u0069\u0309":       0x1EC9,
	"\u0069\u030c":       0x01D0,
	"\u0069\u030f":       0x0209,
	"\u0069\u0311":       0x020B,
	"\u0069\u0323":       0x1ECB,
	"\u0069\u0328":       0x012F,
	"\u0069\u0330":       0x1E2D,
	"\u006a\u0302":       0x0135,
	"\u006a\u030c":       0x01F0,
	"\u006b\u0301":       0x1E31,
	"\u006b\u030c":       0x01E9,
	"\u006b\u0323":       0x1E33,
	"\u006b\u0327":       0x0137,
	"\u006b\u0331":       0x1E35,
	"\u006c\u0301":       0x013A,
	"\u006c\u030c":       0x013E,
	"\u006c\u0323":       0x1E37,
	"\u006c\u0327":       0x013C,
	"\u006c\u032d":       0x1E3D,
	"\u006c\u0331":       0x1E3B,
	"\u006d\u0301":       0x1E3F,
	"\u006d\u0307":       0x1E41,
	"\u006d\u0323":       0x1E43,
	"\u006e\u0300":       0x01F9,
	"\u006e\u0301":       0x0144,
	"\u006e\u0303":       0x00F1,
	"\u006e\u0307":       0x1E45,
	"\u006e\u030c":       0x0148,
	"\u006e\u0323":       0x1E47,
	"\u006e\u0327":       0x0146,
	"\u006e\u032d":       0x1E4B,
	"\u006e\u0331":       0x1E49,
	"\u006f\u0300":       0x00F2,
	"\u006f\u0301":       0x00F3,
	"\u006f\u0302":       0x00F4,
	"\u006f\u0303":       0x00F5,
	"\u006f\u0304":       0x014D,
	"\u006f\u0306":       0x014F,
	"\u006f\u0307":       0x022F,
	"\u006f\u0308":       0x00F6,
	"\u006f\u0309":       0x1ECF,
	"\u006f\u030b":       0x0151,
	"\u006f\u030c":       0x01D2,
	"\u006f\u030f":       0x020D,
	"\u006f\u0311":       0x020F,
	"\u006f\u031b":       0x01A1,
	"\u006f\u0323":       0x1ECD,
	"\u006f\u0328":       0x01EB,
	"\u0070\u0301":       0x1E55,
	"\u0070\u0307":       0x1E57,
	"\u0072\u0301":       0x0155,
	"\u0072\u0307":       0x1E59,
	"\u0072\u030c":       0x0159,
	"\u0072\u030f":       0x0211,
	"\u0072\u0311":       0x0213,
	"\u0072\u0323":       0x1E5B,
	"\u0072\u0327":       0x0157,
	"\u0072\u0331":       0x1E5F,
	"\u0073\u0301":       0x015B,
	"\u0073\u0302":       0x015D,
	"\u0073\u0307":       0x1E61,
	"\u0073\u030c":       0x0161,
	"\u0073\u0323":       0x1E63,
	"\u0073\u0326":       0x0219,
	"\u0073\u0327":       0x015F,
	"\u0074\u0307":       0x1E6B,
	"\u0074\u0308":       0x1E97,
	"\u0074\u030c":       0x0165,
	"\u0074\u0323":       0x1E6D,
	"\u0074\u0326":       0x021B,
	"\u0074\u0327":       0x0163,
	"\u0074\u032d":       0x1E71,
	"\u0074\u0331":       0x1E6F,
	"\u0075\u0300":       0x00F9,
	"\u0075\u0301":       0x00FA,
	"\u0075\u0302":       0x00FB,
	"\u0075\u0303":       0x0169,
	"\u0075\u0304":       0x016B,
	"\u0075\u0306":       0x016D,
	"\u0075\u0308":       0x00FC,
	"\u0075\u0309":       0x1EE7,
	"\u0075\u030a":       0x016F,
	"\u0075\u030b":       0x0171,
	"\u0075\u030c":       0x01D4,
	"\u0075\u030f":       0x0215,
	"\u0075\u0311":       0x0217,
	"\u0075\u031b":       0x01B0,
	"\u0075\u0323":       0x1EE5,
	"\u0075\u0324":       0x1E73,
	"\u0075\u0328":       0x0173,
	"\u0075\u032d":       0x1E77,
	"\u0075\u0330":       0x1E75,
	"\u0076\u0303":       0x1E7D,
	"\u0076\u0323":       0x1E7F,
	"\u0077\u0300":       0x1E81,
	"\u0077\u0301":       0x1E83,
	"\u0077\u0302":       0x0175,
	"\u0077\u0307":       0x1E87,
	"\u0077\u0308":       0x1E85,
	"\u0077\u030a":       0x1E98,
	"\u0077\u0323":       0x1E89,
	"\u0078\u0307":       0x1E8B,
	"\u0078\u0308":       0x1E8D,
	"\u0079\u0300":       0x1EF3,
	"\u0079\u0301":       0x00FD,
	"\u0079\u0302":       0x0177,
	"\u0079\u0303":       0x1EF9,
	"\u0079\u0304":       0x0233,
	"\u0079\u0307":       0x1E8F,
	"\u0079\u0308":       0x00FF,
	"\u0079\u0309":       0x1EF7,
	"\u0079\u030a":       0x1E99,
	"\u0079\u0323":       0x1EF5,
	"\u007a\u0301":       0x017A,
	"\u007a\u0302":       0x1E91,
	"\u007a\u0307":       0x017C,
	"\u007a\u030c":       0x017E,
	"\u007a\u0323":       0x1E93,
	"\u007a\u0331":       0x1E95,
	"\u0041\u0302\u0300": 0x1EA6,
	"\u0041\u0302\u0301": 0x1EA4,
	"\u0041\u0302\u0303": 0x1EAA,
	"\u0041\u0302\u0309": 0x1EA8,
	"\u0041\u0306\u0300": 0x1EB0,
	"\u0041\u0306\u0301": 0x1EAE,
	"\u0041\u0306\u0303": 0x1EB4,
	"\u0041\u0306\u0309": 0x1EB2,
	"\u0041\u0307\u0304": 0x01E0,
	"\u0041\u0308\u0304": 0x01DE,
	"\u0041\u030a\u0301": 0x01FA,
	"\u0041\u0323\u0302": 0x1EAC,
	"\u0041\u0323\u0306": 0x1EB6,
	"\u0043\u0327\u0301": 0x1E08,
	"\u0045\u0302\u0300": 0x1EC0,
	"\u0045\u0302\u0301": 0x1EBE,
	"\u0045\u0302\u0303": 0x1EC4,
	"\u0045\u0302\u0309": 0x1EC2,
	"\u0045\u0304\u0300": 0x1E14,
	"\u0045\u0304\u0301": 0x1E16,
	"\u0045\u0323\u0302": 0x1EC6,
	"\u0045\u0327\u0306": 0x1E1C,
	"\u0049\u0308\u0301": 0x1E2E,
	"\u004c\u0323\u0304": 0x1E38,
	"\u004f\u0302\u0300": 0x1ED2,
	"\u004f\u0302\u0301": 0x1ED0,
	"\u004f\u0302\u0303": 0x1ED6,
	"\u004f\u0302\u0309": 0x1ED4,
	"\u004f\u0303\u0301": 0x1E4C,
	"\u004f\u0303\u0304": 0x022C,
	"\u004f\u0303\u0308": 0x1E4E,
	"\u004f\u0304\u0300": 0x1E50,
	"\u004f\u0304\u0301": 0x1E52,
	"\u004f\u0307\u0304": 0x0230,
	"\u004f\u0308\u0304": 0x022A,
	"\u004f\u031b\u0300": 0x1EDC,
	"\u004f\u031b\u0301": 0x1EDA,
	"\u004f\u031b\u0303": 0x1EE0,
	"\u004f\u031b\u0309": 0x1EDE,
	"\u004f\u031b\u0323": 0x1EE2,
	"\u004f\u0323\u0302": 0x1ED8,
	"\u004f\u0328\u0304": 0x01EC,
	"\u0052\u0323\u0304": 0x1E5C,
	"\u0053\u0301\u0307": 0x1E64,
	"\u0053\u030c\u0307": 0x1E66,
	"\u0053\u0323\u0307": 0x1E68,
	"\u0055\u0303\u0301": 0x1E78,
	"\u0055\u0304\u0308": 0x1E7A,
	"\u0055\u0308\u0300": 0x01DB,
	"\u0055\u0308\u0301": 0x01D7,
	"\u0055\u0308\u0304": 0x01D5,
	"\u0055\u0308\u030c": 0x01D9,
	"\u0055\u031b\u0300": 0x1EEA,
	"\u0055\u031b\u0301": 0x1EE8,
	"\u0055\u031b\u0303": 0x1EEE,
	"\u0055\u031b\u0309": 0x1EEC,
	"\u0055\u031b\u0323": 0x1EF0,
	"\u0061\u0302\u0300": 0x1EA7,
	"\u0061\u0302\u0301": 0x1EA5,
	"\u0061\u0302\u0303": 0x1EAB,
	"\u0061\u0302\u0309": 0x1EA9,
	"\u0061\u0306\u0300": 0x1EB1,
	"\u0061\u0306\u0301": 0x1EAF,
	"\u0061\u0306\u0303": 0x1EB5,
	"\u0061\u0306\u0309": 0x1EB3,
	"\u0061\u0307\u0304": 0x01E1,
	"\u0061\u0308\u0304": 0x01DF,
	"\u0061\u030a\u0301": 0x01FB,
	"\u0061\u0323\u0302": 0x1EAD,
	"\u0061\u0323\u0306": 0x1EB7,
	"\u0063\u0327\u0301": 0x1E09,
	"\u0065\u0302\u0300": 0x1EC1,
	"\u0065\u0302\u0301": 0x1EBF,
	"\u0065\u0302\u0303": 0x1EC5,
	"\u0065\u0302\u0309": 0x1EC3,
	"\u0065\u0304\u0300": 0x1E15,
	"\u0065\u0304\u0301": 0x1E17,
	"\u0065\u0323\u0302": 0x1EC7,
	"\u0065\u0327\u0306": 0x1E1D,
	"\u0069\u0308\u0301": 0x1E2F,
	"\u006c\u0323\u0304": 0x1E39,
	"\u006f\u0302\u0300": 0x1ED3,
	"\u006f\u0302\u0301": 0x1ED1,
	"\u006f\u0302\u0303": 0x1ED7,
	"\u006f\u0302\u0309": 0x1ED5,
	"\u006f\u0303\u0301": 0x1E4D,
	"\u006f\u0303\u0304": 0x022D,
	"\u006f\u0303\u0308": 0x1E4F,
	"\u006f\u0304\u0300": 0x1E51,
	"\u006f\u0304\u0301": 0x1E53,
	"\u006f\u0307\u0304": 0x0231,
	"\u006f\u0308\u0304": 0x022B,
	"\u006f\u031b\u0300": 0x1EDD,
	"\u006f\u031b\u0301": 0x1EDB,
	"\u006f\u031b\u0303": 0x1EE1,
	"\u006f\u031b\u0309": 0x1EDF,
	"\u006f\u031b\u0323": 0x1EE3,
	"\u006f\u0323\u0302": 0x1ED9,
	"\u006f\u0328\u0304": 0x01ED,
	"\u0072\u0323\u0304": 0x1E5D,
	"\u0073\u0301\u0307": 0x1E65,
	"\u0073\u030c\u0307": 0x1E67,
	"\u0073\u0323\u0307": 0x1E69,
	"\u0075\u0303\u0301": 0x1E79,
	"\u0075\u0304\u0308": 0x1E7B,
	"\u0075\u0308\u0300": 0x01DC,
	"\u0075\u0308\u0301": 0x01D8,
	"\u0075\u0308\u0304": 0x01D6,
	"\u0075\u0308\u030c": 0x01DA,
	"\u0075\u031b\u0300": 0x1EEB,
	"\u0075\u031b\u0301": 0x1EE9,
	"\u0075\u031b\u0303": 0x1EEF,
	"\u0075\u031b\u0309": 0x1EED,
	"\u0075\u031b\u0323": 0x1EF1,
}

// nfcLiteMarks is the set of combining-mark runes nfcLiteTable's keys use
// in second or third position — generated alongside the table above, not
// hand-picked. composeNFCLite uses it to decide, cheaply, whether a rune
// might extend a composition at all before trying a table lookup.
var nfcLiteMarks = map[rune]bool{
	0x0300: true, // COMBINING GRAVE ACCENT
	0x0301: true, // COMBINING ACUTE ACCENT
	0x0302: true, // COMBINING CIRCUMFLEX ACCENT
	0x0303: true, // COMBINING TILDE
	0x0304: true, // COMBINING MACRON
	0x0306: true, // COMBINING BREVE
	0x0307: true, // COMBINING DOT ABOVE
	0x0308: true, // COMBINING DIAERESIS
	0x0309: true, // COMBINING HOOK ABOVE
	0x030A: true, // COMBINING RING ABOVE
	0x030B: true, // COMBINING DOUBLE ACUTE ACCENT
	0x030C: true, // COMBINING CARON
	0x030F: true, // COMBINING DOUBLE GRAVE ACCENT
	0x0311: true, // COMBINING INVERTED BREVE
	0x031B: true, // COMBINING HORN
	0x0323: true, // COMBINING DOT BELOW
	0x0324: true, // COMBINING DIAERESIS BELOW
	0x0325: true, // COMBINING RING BELOW
	0x0326: true, // COMBINING COMMA BELOW
	0x0327: true, // COMBINING CEDILLA
	0x0328: true, // COMBINING OGONEK
	0x032D: true, // COMBINING CIRCUMFLEX ACCENT BELOW
	0x032E: true, // COMBINING BREVE BELOW
	0x0330: true, // COMBINING TILDE BELOW
	0x0331: true, // COMBINING MACRON BELOW
}

// composeNFCLite recomposes a base Latin letter followed by one or two
// combining marks into the single precomposed rune Unicode NFC would use,
// via nfcLiteTable — greedy, longest match first at each position, so a
// three-rune Vietnamese sequence (base + modifier + tone, e.g. "e" +
// COMBINING CIRCUMFLEX + COMBINING ACUTE -> "ế") is not left half-composed
// by matching only the first two runes.
//
// # The one assumption this makes, stated plainly
//
// The marks following a base letter are assumed to already be in the SAME
// canonical combining-class order Unicode's own NFD decomposition would
// produce (this is what the generator in nfclite.go's own doc comment
// verified when it built the table: every key IS an NFD decomposition, in
// NFD's own order). Real IME and OS text-input pipelines produce exactly
// that order — measured behaviour, not merely convenient — but a sequence
// that arrives with two marks TRANSPOSED from that order will not match any
// table entry and passes through unrecomposed. golang.org/x/text/unicode/norm
// reorders by combining class before composing and would not have this gap;
// see this file's own top comment for why that dependency is not taken here.
//
// A rune that is not itself Latin ASCII, or not immediately followed by a
// mark this table knows about, is copied through unchanged — this function
// is a no-op on text that carries no decomposed Latin diacritics at all,
// which is the common case for most delivered text.
func composeNFCLite(s string) string {
	if s == "" {
		return s
	}
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(rs); {
		if i+1 < len(rs) && nfcLiteMarks[rs[i+1]] {
			// Try base+2-marks first (longest match), then base+1-mark.
			if i+2 < len(rs) && nfcLiteMarks[rs[i+2]] {
				if composed, ok := nfcLiteTable[string(rs[i:i+3])]; ok {
					b.WriteRune(composed)
					i += 3
					continue
				}
			}
			if composed, ok := nfcLiteTable[string(rs[i:i+2])]; ok {
				b.WriteRune(composed)
				i += 2
				continue
			}
		}
		b.WriteRune(rs[i])
		i++
	}
	return b.String()
}

// normalizeForMatch prepares text for the composer-region comparison
// confirmLandedV2 makes (terminalpath2.go): compose-normalise (see
// composeNFCLite) then drop every whitespace rune.
//
// Whitespace is stripped, not merely collapsed, because the differences this
// comparison must not trip over are not confined to interior runs of spaces:
// composerText already joins a wrapped continuation row onto the previous
// one with a single space the source text never had (colab-fleet's own
// classify.go), the driver's wake key appends one trailing space after the
// text it submits, and Claude Code's own word-wrap (D1 in the round-1
// research this change answers) breaks a long line at a space and drops it,
// then indents the continuation by two more. None of those are meaningful
// differences in the text a human or an agent would read; all three
// disappear under "remove every whitespace rune", and no narrower rule
// removes all three at once without removing meaningful content too.
func normalizeForMatch(s string) string {
	s = composeNFCLite(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
