package claudecli

import "os"

func writeFile(path, body string, mode os.FileMode) error {
	return os.WriteFile(path, []byte(body), mode)
}
