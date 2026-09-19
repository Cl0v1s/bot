package tools

import "testing"

func TestCheckDangerousShellCommand(t *testing.T) {
	blocked := []string{
		`rm -rf /tmp/foo`,
		`rm -fr /tmp/foo`,
		`sudo rm -rf /`,
		`rm --recursive --force /tmp/foo`,
		`rm -f /tmp/foo.txt`,
		`echo hi && rm -rf /tmp/foo`,
		`mkfs.ext4 /dev/sda1`,
		`dd if=/dev/zero of=/dev/sda`,
		`echo x > /dev/sda`,
		`wipefs /dev/sda`,
		`shred /dev/sda`,
		`shutdown -h now`,
		`reboot`,
		`:(){ :|:& };:`,
		`chmod -R 777 /`,
		`chown -R nobody /`,
	}
	for _, cmd := range blocked {
		if dangerous, _, _ := checkDangerousShellCommand(cmd); !dangerous {
			t.Errorf("expected %q to be blocked, but it was not", cmd)
		}
	}

	allowed := []string{
		`ls -la`,
		`echo "hello world"`,
		`curl -f https://example.com`,
		`git status`,
		`rm singlefile.txt`,
		`rm -i important.txt`,
		`grep -rn "TODO" .`,
		`find . -name "*.go"`,
		`echo test && curl -f https://example.com`,
	}
	for _, cmd := range allowed {
		if dangerous, label, seg := checkDangerousShellCommand(cmd); dangerous {
			t.Errorf("expected %q to be allowed, but it was blocked (%s, segment %q)", cmd, label, seg)
		}
	}
}
