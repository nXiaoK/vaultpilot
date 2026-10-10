// vprescue decrypts an offline recovery kit and downloaded catalog without a database or running application.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"vaultpilot/internal/engine"
	"vaultpilot/internal/platform"
)

func run() error {
	kitPath := flag.String("kit", "", "encrypted recovery kit JSON")
	catalogPath := flag.String("catalog", "", "downloaded committed .vpc catalog")
	commitPath := flag.String("commit", "", "matching authenticated .ok completion certificate")
	passwordPath := flag.String("password-file", "", "0600 file containing recovery-kit protection password")
	outputPath := flag.String("out", "", "new private output directory (must not already exist)")
	flag.Parse()
	if *kitPath == "" || *catalogPath == "" || *commitPath == "" || *passwordPath == "" || *outputPath == "" {
		return errors.New("需要 -kit -catalog -commit -password-file -out；密码不能通过命令参数传入")
	}
	info, err := os.Lstat(*passwordPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("密码文件必须是普通文件且权限为 0600")
	}
	password, err := os.ReadFile(*passwordPath)
	if err != nil {
		return err
	}
	defer func() {
		for i := range password {
			password[i] = 0
		}
	}()
	kit, err := os.ReadFile(*kitPath)
	if err != nil {
		return err
	}
	if len(kit) > 16<<20 {
		return errors.New("恢复包过大")
	}
	payload, err := platform.UnlockRecoveryKit(kit, strings.TrimRight(string(password), "\r\n"))
	if err != nil {
		return err
	}
	master, err := base64.StdEncoding.DecodeString(payload.MasterKey)
	if err != nil {
		return err
	}
	keys := &platform.Keyring{}
	if err = keys.SetMaster(master); err != nil {
		return err
	}
	for i := range master {
		master[i] = 0
	}
	defer keys.Lock()
	certificate, err := os.ReadFile(*commitPath)
	if err != nil {
		return err
	}
	opened, err := keys.Open(certificate)
	if err != nil {
		return errors.New("完成证书认证失败")
	}
	var marker struct {
		Format           int      `json:"format"`
		State            string   `json:"state"`
		PreparedReplicas int      `json:"prepared_replicas"`
		VaultID          string   `json:"vault_id"`
		CommitID         string   `json:"commit_id"`
		SHA256           string   `json:"sha256"`
		Drives           []string `json:"drives"`
	}
	if err = json.Unmarshal(opened, &marker); err != nil {
		return err
	}
	if marker.Format != 1 || marker.VaultID != payload.VaultID || marker.State != "committed" {
		return errors.New("完成证书库身份或协议无效")
	}
	catalog, err := os.Open(*catalogPath)
	if err != nil {
		return err
	}
	digest := sha256.New()
	_, err = io.Copy(digest, catalog)
	catalog.Close()
	if err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != marker.SHA256 {
		return errors.New("控制目录与认证完成证书的摘要不一致")
	}
	out, err := filepath.Abs(*outputPath)
	if err != nil {
		return err
	}
	if err = os.Mkdir(out, 0700); err != nil {
		return fmt.Errorf("必须指定一个不存在的新目录: %w", err)
	}
	completed := false
	defer func() {
		if !completed {
			os.RemoveAll(out)
		}
	}()
	archivePath := filepath.Join(out, "catalog.zip")
	defer os.Remove(archivePath)
	if err = engine.DecryptCatalog(keys, *catalogPath, archivePath); err != nil {
		return err
	}
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	var control struct {
		Format       int    `json:"format"`
		VaultID      string `json:"vault_id"`
		CommitID     string `json:"commit_id"`
		Replicas     int    `json:"replicas"`
		Repositories []struct {
			ID       string `json:"id"`
			Password string `json:"password"`
		} `json:"repositories"`
	}
	found := false
	for _, file := range archive.File {
		if file.Name != "control.json" {
			continue
		}
		reader, e := file.Open()
		if e != nil {
			return e
		}
		raw, e := io.ReadAll(io.LimitReader(reader, 32<<20))
		reader.Close()
		if e != nil {
			return e
		}
		if e = json.Unmarshal(raw, &control); e != nil {
			return e
		}
		if control.VaultID != payload.VaultID || control.CommitID != marker.CommitID || control.Format != 1 {
			return errors.New("云端目录身份、版本与完成证书不一致")
		}
		drives := map[string]bool{}
		for _, id := range marker.Drives {
			drives[id] = true
		}
		if control.Replicas < 1 || marker.PreparedReplicas != control.Replicas || len(drives) < control.Replicas {
			return errors.New("完成证书未确认必要的不同Drive准备副本")
		}
		if e = os.WriteFile(filepath.Join(out, "control.json"), raw, 0600); e != nil {
			return e
		}
		found = true
		break
	}
	if !found {
		return errors.New("catalog 中缺少 control.json")
	}
	validID := regexp.MustCompile(`^[a-f0-9]{32}$`)
	for _, repo := range control.Repositories {
		if !validID.MatchString(repo.ID) {
			return errors.New("仓库登记标识无效")
		}
		sealed, e := base64.StdEncoding.DecodeString(repo.Password)
		if e != nil {
			return e
		}
		secret, e := keys.Open(sealed)
		if e != nil {
			return e
		}
		e = os.WriteFile(filepath.Join(out, repo.ID+".password"), secret, 0600)
		for i := range secret {
			secret[i] = 0
		}
		if e != nil {
			return e
		}
	}
	if err = os.Mkdir(filepath.Join(out, "points"), 0700); err != nil {
		return err
	}
	for _, file := range archive.File {
		if !strings.HasPrefix(file.Name, "points/") || !strings.HasSuffix(file.Name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(file.Name, "points/"), ".jsonl")
		if !validID.MatchString(id) {
			return errors.New("恢复点登记标识无效")
		}
		reader, e := file.Open()
		if e != nil {
			return e
		}
		dest, e := os.OpenFile(filepath.Join(out, "points", id+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			reader.Close()
			return e
		}
		_, e = io.Copy(dest, reader)
		reader.Close()
		closeErr := dest.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	completed = true
	fmt.Printf("已解密备份库 %s 的登记、%d 个仓库密码及恢复点路径清单到 %s\n请保护这些明文恢复资料，并在恢复完成后安全处理。\n", payload.VaultID, len(control.Repositories), out)
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "恢复资料解密失败:", err)
		os.Exit(1)
	}
}
