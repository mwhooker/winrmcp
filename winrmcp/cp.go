package winrmcp

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"sync"

	"github.com/masterzen/winrm"
	"github.com/nu7hatch/gouuid"
)

type uploadJob struct {
	uploadPath string
	chunk      []byte
	n          int
}

func doCopy(client *winrm.Client, config *Config, in io.Reader, toPath string) error {
	uniquePart, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("Error generating unique filename: %v", err)
	}

	tempFile := fmt.Sprintf("%%%%TEMP%%%%\\winrmcp-%s-%%d.tmp", uniquePart)
	//tempFile := fmt.Sprintf("C:\\Users\\vagrant\\winrmcp-%s-%%d", uniquePart)

	// Create a buffer to write our archive to.
	buf := new(bytes.Buffer)

	b64Enc := base64.NewEncoder(base64.StdEncoding, buf)

	// Create a new zip archive.
	w := zip.NewWriter(b64Enc)
	f, err := w.Create(path.Base(toPath))
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, in); err != nil {
		return err
	}
	// Make sure to check the error on Close.
	if err := w.Close(); err != nil {
		return err
	}

	if err := b64Enc.Close(); err != nil {
		return err
	}

	jobs := make(chan *uploadJob, 2000)
	concurrentUploads := 30
	done := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < concurrentUploads; i++ {
		go func() {
			for {
				select {
				case j := <-jobs:
					if err := retry(func() error {
						return writeChunk(client, j.uploadPath, string(j.chunk[:j.n]))
					}, 3); err != nil {
						for i := 0; i < concurrentUploads; i++ {
							done <- struct{}{}
						}
						log.Printf("error appending: %s", err)
					}
					wg.Done()
				case <-done:
					return
				}

			}
		}()
	}

	for i := 0; ; i++ {
		tempPathChunk := fmt.Sprintf(tempFile, i)
		cs := chunkSize(tempPathChunk)
		chunk := make([]byte, cs)
		n, err := buf.Read(chunk)

		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}

		log.Printf("making job %s. Uploading to %s", i, tempPathChunk)
		wg.Add(1)
		jobs <- &uploadJob{tempPathChunk, chunk, n}
	}

	wg.Wait()

	/*
		done := false
		for !done {
			done, err = uploadChunks(client, tempPath, config.MaxOperationsPerShell, buf)
			if err != nil {
				return fmt.Errorf("Error uploading file to %s: %v", tempPath, err)
			}
		}

		if os.Getenv("WINRMCP_DEBUG") != "" {
			log.Printf("Moving file from %s to %s", tempPath, toPath)
		}

		err = restoreContent(client, tempPath, toPath)
		if err != nil {
			return fmt.Errorf("Error restoring file from %s to %s: %v", tempPath, toPath, err)
		}

		if os.Getenv("WINRMCP_DEBUG") != "" {
			log.Printf("Removing temporary file %s", tempPath)
		}

		err = cleanupContent(client, tempPath)
		if err != nil {
			return fmt.Errorf("Error removing temporary file %s: %v", tempPath, err)
		}
	*/

	return nil
}

type RetryableFunc func() error

func retry(f RetryableFunc, maxTries int) error {
	var err error
	for i := 0; i < maxTries; i++ {
		err = f()
		if err != nil {
			continue
		}
		return nil
	}
	return fmt.Errorf("Retries exhausted: %s", err.Error())
}

func writeChunk(client *winrm.Client, filePath, content string) error {
	scmd := fmt.Sprintf(`echo "%s" > "%s"`, content, filePath)

	//log.Printf("Appending content: (len=%d), %s", len(scmd), scmd)

	out, errs, code, err := client.RunWithString(scmd, "")
	fmt.Printf("out: %s\nerrs: %s\n", out, errs)

	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("upload operation returned code=%d", code)
	}

	return nil
}

func chunkSize(filePath string) int {
	// Upload the file in chunks to get around the Windows command line size limit.

	//return 8192 - len(filePath)
	return 7500 - len(filePath)
}

func restoreContent(client *winrm.Client, fromPath, toPath string) error {
	shell, err := client.CreateShell()
	if err != nil {
		return err
	}

	defer shell.Close()
	script := fmt.Sprintf(`
		$tmp_file_path = [System.IO.Path]::GetFullPath("%s")
		$dest_file_path = [System.IO.Path]::GetFullPath("%s".Trim("'"))
		if (Test-Path $dest_file_path) {
			rm $dest_file_path
		}
		else {
			$dest_dir = ([System.IO.Path]::GetDirectoryName($dest_file_path))
			New-Item -ItemType directory -Force -ErrorAction SilentlyContinue -Path $dest_dir | Out-Null
		}

		if (Test-Path $tmp_file_path) {
			$reader = [System.IO.File]::OpenText($tmp_file_path)
			$writer = [System.IO.File]::OpenWrite($dest_file_path)
			try {
				for(;;) {
					$base64_line = $reader.ReadLine()
					if ($base64_line -eq $null) { break }
					$bytes = [System.Convert]::FromBase64String($base64_line)
					$writer.write($bytes, 0, $bytes.Length)
				}
			}
			finally {
				$reader.Close()
				$writer.Close()
			}
		} else {
			echo $null > $dest_file_path
		}
	`, fromPath, toPath)

	cmd, err := shell.Execute(winrm.Powershell(script))
	if err != nil {
		return err
	}
	defer cmd.Close()

	var wg sync.WaitGroup
	copyFunc := func(w io.Writer, r io.Reader) {
		defer wg.Done()
		io.Copy(w, r)
	}

	wg.Add(2)
	go copyFunc(os.Stdout, cmd.Stdout)
	go copyFunc(os.Stderr, cmd.Stderr)

	cmd.Wait()
	wg.Wait()

	if cmd.ExitCode() != 0 {
		return fmt.Errorf("restore operation returned code=%d", cmd.ExitCode())
	}
	return nil
}

func cleanupContent(client *winrm.Client, filePath string) error {
	shell, err := client.CreateShell()
	if err != nil {
		return err
	}

	defer shell.Close()
	script := fmt.Sprintf(`Remove-Item %s -ErrorAction SilentlyContinue`, filePath)

	cmd, err := shell.Execute(winrm.Powershell(script))
	if err != nil {
		return err
	}
	defer cmd.Close()

	var wg sync.WaitGroup
	copyFunc := func(w io.Writer, r io.Reader) {
		defer wg.Done()
		io.Copy(w, r)
	}

	wg.Add(2)
	go copyFunc(os.Stdout, cmd.Stdout)
	go copyFunc(os.Stderr, cmd.Stderr)

	cmd.Wait()
	wg.Wait()

	if cmd.ExitCode() != 0 {
		return fmt.Errorf("cleanup operation returned code=%d", cmd.ExitCode())
	}
	return nil
}
