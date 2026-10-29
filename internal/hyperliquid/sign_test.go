package hyperliquid

import (
	"encoding/hex"
	"testing"
)

// The vectors are the official SDK's signing tests, so a signature that
// matches here is one the exchange accepts.

const testKey = "0x0123456789012345678901234567890123456789012345678901234567890123"

func signer(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func expect(t *testing.T, got Signature, err error, r, s string, v int) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if got.R != r || got.S != s || got.V != v {
		t.Fatalf("got r=%s s=%s v=%d\nwant r=%s s=%s v=%d", got.R, got.S, got.V, r, s, v)
	}
}

func TestActionHashMatchesTheSDK(t *testing.T) {
	a := orderAction([]Order{{Asset: 4, Buy: true, Size: 0.0147, Price: 1670.1, Tif: Ioc}}, GroupNone, nil)
	h, err := actionHash(a, "", 1677777606040, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h); got != "0fcbeda5ae3c4950a548021552a4fea2226858c4453571bf3f24ba017eac2908" {
		t.Fatalf("hash %s", got)
	}
}

func TestL1SigningMatchesTheSDK(t *testing.T) {
	s := signer(t)
	dummy := omap{{"type", "dummy"}, {"num", uint64(100000000000)}}
	sig, err := s.signL1(dummy, "", 0, nil, true)
	expect(t, sig, err, "0x53749d5b30552aeb2fca34b530185976545bb22d0b3ce6f62e31be961a59298", "0x755c40ba9bf05223521753995abb2f73ab3229be8ec921f350cb447e384d8ed8", 27)
	sig, err = s.signL1(dummy, "", 0, nil, false)
	expect(t, sig, err, "0x542af61ef1f429707e3c76c5293c80d01f74ef853e34b76efffcb57e574f9510", "0x17b8b32f086e8cdede991f1e2c529f5dd5297cbe8128500e00cbaf766204a613", 28)

	vault := "0x1719884eb866cb12b2287399b15f7db5e7d775ea"
	sig, err = s.signL1(dummy, vault, 0, nil, true)
	expect(t, sig, err, "0x3c548db75e479f8012acf3000ca3a6b05606bc2ec0c29c50c515066a326239", "0x4d402be7396ce74fbba3795769cda45aec00dc3125a984f2a9f23177b190da2c", 28)
	sig, err = s.signL1(dummy, vault, 0, nil, false)
	expect(t, sig, err, "0xe281d2fb5c6e25ca01601f878e4d69c965bb598b88fac58e475dd1f5e56c362b", "0x7ddad27e9a238d045c035bc606349d075d5c5cd00a6cd1da23ab5c39d4ef0f60", 27)
}

func TestOrderSigningMatchesTheSDK(t *testing.T) {
	s := signer(t)
	order := Order{Asset: 1, Buy: true, Size: 100, Price: 100, Tif: Gtc}
	sig, err := s.signL1(orderAction([]Order{order}, GroupNone, nil), "", 0, nil, true)
	expect(t, sig, err, "0xd65369825a9df5d80099e513cce430311d7d26ddf477f5b3a33d2806b100d78e", "0x2b54116ff64054968aa237c20ca9ff68000f977c93289157748a3162b6ea940e", 28)
	sig, err = s.signL1(orderAction([]Order{order}, GroupNone, nil), "", 0, nil, false)
	expect(t, sig, err, "0x82b2ba28e76b3d761093aaded1b1cdad4960b3af30212b343fb2e6cdfa4e3d54", "0x6b53878fc99d26047f4d7e8c90eb98955a109f44209163f52d8dc4278cbbd9f5", 27)

	order.Cloid = "0x00000000000000000000000000000001"
	sig, err = s.signL1(orderAction([]Order{order}, GroupNone, nil), "", 0, nil, true)
	expect(t, sig, err, "0x41ae18e8239a56cacbc5dad94d45d0b747e5da11ad564077fcac71277a946e3", "0x3c61f667e747404fe7eea8f90ab0e76cc12ce60270438b2058324681a00116da", 27)
	sig, err = s.signL1(orderAction([]Order{order}, GroupNone, nil), "", 0, nil, false)
	expect(t, sig, err, "0xeba0664bed2676fc4e5a743bf89e5c7501aa6d870bdb9446e122c9466c5cd16d", "0x7f3e74825c9114bc59086f1eebea2928c190fdfbfde144827cb02b85bbe90988", 28)

	stop := Order{Asset: 1, Buy: true, Size: 100, Price: 100, Trigger: &Trigger{Price: 103, Market: true, Kind: "sl"}}
	sig, err = s.signL1(orderAction([]Order{stop}, GroupNone, nil), "", 0, nil, true)
	expect(t, sig, err, "0x98343f2b5ae8e26bb2587daad3863bc70d8792b09af1841b6fdd530a2065a3f9", "0x6b5bb6bb0633b710aa22b721dd9dee6d083646a5f8e581a20b545be6c1feb405", 27)
	sig, err = s.signL1(orderAction([]Order{stop}, GroupNone, nil), "", 0, nil, false)
	expect(t, sig, err, "0x971c554d917c44e0e1b6cc45d8f9404f32172a9d3b3566262347d0302896a2e4", "0x206257b104788f80450f8e786c329daa589aa0b32ba96948201ae556d5637eac", 28)
}

func TestOtherActionsMatchTheSDK(t *testing.T) {
	s := signer(t)
	sub := omap{{"type", "createSubAccount"}, {"name", "example"}}
	sig, err := s.signL1(sub, "", 0, nil, true)
	expect(t, sig, err, "0x51096fe3239421d16b671e192f574ae24ae14329099b6db28e479b86cdd6caa7", "0xb71f7d293af92d3772572afb8b102d167a7cef7473388286bc01f52a5c5b423", 27)

	transfer := omap{{"type", "subAccountTransfer"}, {"subAccountUser", "0x1d9470d4b963f552e6f671a81619d395877bf409"}, {"isDeposit", true}, {"usd", 10}}
	sig, err = s.signL1(transfer, "", 0, nil, false)
	expect(t, sig, err, "0xe26574013395ad55ee2f4e0575310f003c5bb3351b5425482e2969fa51543927", "0xefb08999196366871f919fd0e138b3a7f30ee33e678df7cfaf203e25f0a4278", 28)

	sig, err = s.signL1(omap{{"type", "scheduleCancel"}}, "", 0, nil, true)
	expect(t, sig, err, "0x6cdfb286702f5917e76cd9b3b8bf678fcc49aec194c02a73e6d4f16891195df9", "0x6557ac307fa05d25b8d61f21fb8a938e703b3d9bf575f6717ba21ec61261b2a0", 27)
	sig, err = s.signL1(omap{{"type", "scheduleCancel"}, {"time", 123456789}}, "", 0, nil, false)
	expect(t, sig, err, "0x4e4f2dbd4107c69783e251b7e1057d9f2b9d11cee213441ccfa2be63516dc5bc", "0x706c656b23428c8ba356d68db207e11139ede1670481a9e01ae2dfcdb0e1a678", 27)
}

func TestUserSigningMatchesTheSDK(t *testing.T) {
	s := signer(t)
	fields := []field{{"hyperliquidChain", "string"}, {"destination", "string"}, {"amount", "string"}, {"time", "uint64"}}
	msg := omap{{"type", "usdSend"}, {"signatureChainId", signatureChainID}, {"hyperliquidChain", "Testnet"},
		{"destination", "0x5e9ee1089755c3435139848e47e6635505d5a13a"}, {"amount", "1"}, {"time", uint64(1687816341423)}}
	sig, err := s.signUser("HyperliquidTransaction:UsdSend", fields, msg)
	expect(t, sig, err, "0x637b37dd731507cdd24f46532ca8ba6eec616952c56218baeff04144e4a77073", "0x11a6a24900e6e314136d2592e2f8d502cd89b7c15b198e1bee043c9589f9fad7", 27)
	sig, err = s.signUser("HyperliquidTransaction:Withdraw", fields, msg)
	expect(t, sig, err, "0x8363524c799e90ce9bc41022f7c39b4e9bdba786e5f9c72b20e43e1462c37cf9", "0x58b1411a775938b83e29182e8ef74975f9054c8e97ebf5ec2dc8d51bfc893881", 28)
}

func TestWireNumbers(t *testing.T) {
	for x, want := range map[float64]string{0.0147: "0.0147", 1670.1: "1670.1", 100: "100", 1e-8: "0.00000001", 123456.5: "123456.5", 0: "0"} {
		if got := wire(x); got != want {
			t.Errorf("wire(%v) = %s, want %s", x, got, want)
		}
	}
	if got := RoundPrice(81452.5*1.03, 5); got != 83896 {
		t.Errorf("RoundPrice BTC = %v", got)
	}
	if got := RoundPrice(0.0123456, 0); got != 0.012346 {
		t.Errorf("RoundPrice small = %v", got)
	}
	if got := RoundSize(0.123456789, 5); got != 0.12345 {
		t.Errorf("RoundSize = %v", got)
	}
}

func TestSignerAddress(t *testing.T) {
	s, key, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	again, err := NewSigner(key)
	if err != nil || again.Address() != s.Address() || len(s.Address()) != 42 {
		t.Fatalf("address %s from key again %v: %v", s.Address(), again, err)
	}
}
