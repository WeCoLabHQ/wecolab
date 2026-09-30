package warden

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The GetObject example from AWS's Signature Version 4 documentation, "Authenticating Requests:
// Using the Authorization Header", with its published signature.
func TestSignAWSExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	now, _ := time.Parse("20060102T150405Z", "20130524T000000Z")
	sign(req, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", now)
	want := "Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, want) || !strings.Contains(got, "SignedHeaders=host;range;x-amz-content-sha256;x-amz-date") {
		t.Fatalf("authorization: %s", got)
	}
}

func TestRegion(t *testing.T) {
	for ep, want := range map[string]string{
		"https://s3.eu-central-003.backblazeb2.com": "eu-central-003",
		"https://s3.us-west-2.amazonaws.com":        "us-west-2",
		"http://vault:9000":                         "us-east-1",
	} {
		if got := (S3{Endpoint: ep}).Region(); got != want {
			t.Errorf("%s: %s", ep, got)
		}
	}
}
