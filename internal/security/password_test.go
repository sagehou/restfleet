package security

import "testing"

func TestPasswordHashRoundTrip(t *testing.T) {
	params := Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}
	encoded, err := HashPassword("correct horse battery staple", params)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyPassword(encoded, "correct horse battery staple")
	if err != nil || !ok {
		t.Fatalf("valid password rejected: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(encoded, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("wrong password accepted")
	}
}

func TestPasswordHashRejectsUnsafePersistedParameters(t *testing.T) {
	tests := []string{
		"$argon2id$v=19$m=262145,t=1,p=1$MTIzNDU2Nzg$MTIzNDU2Nzg5MDEyMzQ1Ng",
		"$argon2id$v=19junk$m=64,t=1,p=1$MTIzNDU2Nzg$MTIzNDU2Nzg5MDEyMzQ1Ng",
		"$argon2id$v=19$m=64,t=1,p=1junk$MTIzNDU2Nzg$MTIzNDU2Nzg5MDEyMzQ1Ng",
	}
	for _, encoded := range tests {
		if ok, err := VerifyPassword(encoded, "irrelevant"); err == nil || ok {
			t.Fatalf("unsafe password hash accepted: %q", encoded)
		}
	}
}
