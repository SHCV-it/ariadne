package core

import "testing"

func TestRedact(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// The bug from production: hyphenated words containing -p<letter>
		// are not passwords.
		{"herdr session attach neuromark-core-pipeline", "herdr session attach neuromark-core-pipeline"},
		{"herdr session attach neuromark-core-p<redacted>", "herdr session attach neuromark-core-p<redacted>"},
		// Ports and port maps are not passwords.
		{"docker run -p 8080:80 nginx", "docker run -p 8080:80 nginx"},
		{"docker run -p8080:80 nginx", "docker run -p8080:80 nginx"},
		{"ssh -p2222 host.example.com", "ssh -p2222 host.example.com"},
		{"kubectl port-forward -p 8080 pod/x", "kubectl port-forward -p 8080 pod/x"},
		// Real attached-password forms are still scrubbed.
		{"mysql -pSecret123", "mysql -p" + RedactMark},
		{"mysql -u root -pS3cret! db", "mysql -u root -p" + RedactMark + "! db"},
		{"mycli -p=hunter2 mydb", "mycli -p" + RedactMark + " mydb"},
		// Existing rules, unchanged behaviour.
		{`curl -H "Authorization: Bearer abc123xyz" x`, `curl -H "Authorization: Bearer ` + RedactMark + " x"},
		{"export OPENAI_API_KEY=sk-abcdef1234567890abcdefzz", "export OPENAI_API_KEY=" + RedactMark},
		{"git clone https://ghp_abcdefghijklmnop.example/x", "git clone https://" + RedactMark + ".example/x"},
		{"cmd --token=abc --password hunter2", "cmd --token=" + RedactMark + " --password " + RedactMark},
		// Boring commands pass through untouched.
		{"git status", "git status"},
		{"kubectl get pods -n kube-system", "kubectl get pods -n kube-system"},
	}
	for _, c := range cases {
		got, _ := Redact(c.in)
		if got != c.want {
			t.Errorf("Redact(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}
