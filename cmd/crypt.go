package cmd

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	rcCrypt "github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// encryption and decryption command format for Crypt driver

// failedFile records one file that could not be encrypted/decrypted.
type failedFile struct {
	src string
	err error
}

type options struct {
	op  string //decrypt or encrypt
	src string //source dir or file
	dst string //out destination

	pwd                string //de/encrypt password
	salt               string
	filenameEncryption string //reference drivers\crypt\meta.go Addition
	dirnameEncryption  string
	filenameEncode     string
	suffix             string

	// failedFiles collects per-file failures so one unreadable file does not
	// abort the whole run; the summary is printed after the walk finishes.
	failedFiles []failedFile
}

var opt options

// CryptCmd represents the crypt command
var CryptCmd = &cobra.Command{
	Use:     "crypt",
	Short:   "Encrypt or decrypt local file or dir",
	Example: `openlist crypt  -s ./src/encrypt/ --op=de --pwd=123456 --salt=345678`,
	Run: func(cmd *cobra.Command, args []string) {
		opt.validate()
		opt.cryptFileDir()
		opt.reportFailed()

	},
}

func init() {
	RootCmd.AddCommand(CryptCmd)
	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// versionCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	CryptCmd.Flags().StringVarP(&opt.src, "src", "s", "", "src file or dir to encrypt/decrypt")
	CryptCmd.Flags().StringVarP(&opt.dst, "dst", "d", "", "dst dir to output,if not set,output to src dir")
	CryptCmd.Flags().StringVar(&opt.op, "op", "", "de or en which stands for decrypt or encrypt")

	CryptCmd.Flags().StringVar(&opt.pwd, "pwd", "", "password used to encrypt/decrypt,if not contain ___Obfuscated___ prefix,will be obfuscated before used")
	CryptCmd.Flags().StringVar(&opt.salt, "salt", "", "salt used to encrypt/decrypt,if not contain ___Obfuscated___ prefix,will be obfuscated before used")
	CryptCmd.Flags().StringVar(&opt.filenameEncryption, "filename-encrypt", "off", "filename encryption mode: off,standard,obfuscate")
	CryptCmd.Flags().StringVar(&opt.dirnameEncryption, "dirname-encrypt", "false", "is dirname encryption enabled:true,false")
	CryptCmd.Flags().StringVar(&opt.filenameEncode, "filename-encode", "base64", "filename encoding mode: base64,base32,base32768")
	CryptCmd.Flags().StringVar(&opt.suffix, "suffix", ".bin", "suffix for encrypted file,default is .bin")
}

func (o *options) validate() {
	if o.src == "" {
		log.Fatal("src can not be empty")
	}
	if o.op != "encrypt" && o.op != "decrypt" && o.op != "en" && o.op != "de" {
		log.Fatal("op must be encrypt or decrypt")
	}
	if o.filenameEncryption != "off" && o.filenameEncryption != "standard" && o.filenameEncryption != "obfuscate" {
		log.Fatal("filename_encryption must be off,standard,obfuscate")
	}
	if o.filenameEncode != "base64" && o.filenameEncode != "base32" && o.filenameEncode != "base32768" {
		log.Fatal("filename_encode must be base64,base32,base32768")
	}

}

func (o *options) cryptFileDir() {
	src, _ := filepath.Abs(o.src)
	log.Infof("src abs is %v", src)

	fileInfo, err := os.Stat(src)
	if err != nil {
		log.Fatalf("reading file/dir %v failed,err:%v", src, err)

	}
	pwd := updateObfusParm(o.pwd)
	salt := updateObfusParm(o.salt)

	//create cipher
	config := configmap.Simple{
		"password":                  pwd,
		"password2":                 salt,
		"filename_encryption":       o.filenameEncryption,
		"directory_name_encryption": o.dirnameEncryption,
		"filename_encoding":         o.filenameEncode,
		"suffix":                    o.suffix,
		"pass_bad_blocks":           "",
	}
	// 不要把 password / salt 打进日志：命令行工具的输出会留在终端和日志里
	log.Infof("config:filename_encryption=%s,directory_name_encryption=%s,filename_encoding=%s,suffix=%s",
		o.filenameEncryption, o.dirnameEncryption, o.filenameEncode, o.suffix)
	cipher, err := rcCrypt.NewCipher(config)
	if err != nil {
		log.Fatalf("create cipher failed,err:%v", err)

	}
	dst := ""
	//check and create dst dir
	if o.dst != "" {
		dst, _ = filepath.Abs(o.dst)
		checkCreateDir(dst)
	}

	// src is file
	if !fileInfo.IsDir() { //file
		if dst == "" {
			dst = filepath.Dir(src)
		}
		if err := o.cryptFile(cipher, src, dst); err != nil {
			o.failedFiles = append(o.failedFiles, failedFile{src: src, err: err})
		}
		return
	}

	// src is dir
	if dst == "" {
		//if src is dir and not set dst dir ,create ${src}_crypt dir as dst dir
		dst = path.Join(filepath.Dir(src), fileInfo.Name()+"_crypt")
	}
	log.Infof("dst : %v", dst)

	dirnameMap := make(map[string]string)
	// 注意：原代码用 filepath.Walk 得到的 p 去 strings.ReplaceAll 绝对路径 src，
	// 再按 os.PathSeparator 切分；这部分逻辑原样保留，只把变量名改准确。
	sep := string(os.PathSeparator)

	filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			// 目录本身读不到（权限/IO 错误）：Walk 无法继续下探，记录后
			// 仍返回 nil，让兄弟目录继续处理，而不是中断整个目录树。
			o.failedFiles = append(o.failedFiles, failedFile{src: p, err: err})
			log.Errorf("get file %v info failed, skip, err:%v", p, err)
			return nil
		}
		if p == src {
			return nil
		}
		log.Infof("current path %v", p)

		// relative path
		rp := strings.ReplaceAll(p, src, "")
		log.Infof("relative path %v", rp)

		rpds := strings.Split(rp, sep)

		// dirNameFailed 在目录名解密失败时置位：本目录无法映射到输出路径。
		dirNameFailed := false

		if info.IsDir() {
			// absolute dst dir for current path
			dd := ""

			if o.dirnameEncryption == "true" {
				if o.op == "encrypt" || o.op == "en" {
					for i := range rpds {
						oname := rpds[i]
						if _, ok := dirnameMap[rpds[i]]; ok {
							rpds[i] = dirnameMap[rpds[i]]
						} else {
							rpds[i] = cipher.EncryptDirName(rpds[i])
							dirnameMap[oname] = rpds[i]
						}
					}
					dd = path.Join(dst, strings.Join(rpds, sep))
				} else {
					for i := range rpds {
						oname := rpds[i]
						if _, ok := dirnameMap[rpds[i]]; ok {
							rpds[i] = dirnameMap[rpds[i]]
						} else {
							dnn, err := cipher.DecryptDirName(rpds[i])
							if err != nil {
								o.failedFiles = append(o.failedFiles, failedFile{
									src: p,
									err: fmt.Errorf("decrypt dir name %q (segment %q): %w", p, rpds[i], err),
								})
								log.Errorf("decrypt dir name %v failed, skip dir %v, err:%v", rpds[i], p, err)
								// 这个 continue 只跳到 rpds 的下一个路径段，
								// 真正「跳过本目录」由下面的 dirNameFailed 完成。
								dirNameFailed = true
								continue
							}
							rpds[i] = dnn
							dirnameMap[oname] = dnn
						}

					}
					dd = path.Join(dst, strings.Join(rpds, sep))
				}

			} else {
				dd = path.Join(dst, rp)
			}

			if dirNameFailed {
				log.Warnf("dir name decrypt failed, skip creating output dir for %v", p)
				return nil
			}
			log.Infof("create output dir %v", dd)
			checkCreateDir(dd)
			return nil
		}

		// file dst dir
		fdd := dst

		if o.dirnameEncryption == "true" {
			for i := range rpds {
				if i == len(rpds)-1 {
					break
				}
				fdd = path.Join(fdd, dirnameMap[rpds[i]])
			}

		} else {
			fdd = path.Join(fdd, strings.Join(rpds[:len(rpds)-1], sep))
		}

		log.Infof("file output dir %v", fdd)
		if err := o.cryptFile(cipher, p, fdd); err != nil {
			o.failedFiles = append(o.failedFiles, failedFile{src: p, err: err})
			log.Errorf("crypt file %v failed, skip, err:%v", p, err)
			return nil // 单文件失败只跳过当前文件，继续遍历
		}
		return nil
	})

}

// reportFailed 打印单文件失败汇总；退出码保持 0（调用方按需改）。
func (o *options) reportFailed() {
	if len(o.failedFiles) == 0 {
		return
	}
	log.Warnf("%d file(s) failed and were skipped:", len(o.failedFiles))
	for i, f := range o.failedFiles {
		log.Warnf("  [%d] %v: %v", i+1, f.src, f.err)
	}
}

func (o *options) cryptFile(cipher *rcCrypt.Cipher, src string, dst string) error {
	fileInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("get file %v info failed: %w", src, err)
	}
	fd, err := os.OpenFile(src, os.O_RDWR, 0666)
	if err != nil {
		return fmt.Errorf("open file %v failed: %w", src, err)
	}
	defer fd.Close()

	var cryptSrcReader io.Reader
	var outFile string
	if o.op == "encrypt" || o.op == "en" {
		filename := fileInfo.Name()
		if o.filenameEncryption != "off" {
			filename = cipher.EncryptFileName(fileInfo.Name())
			log.Infof("encrypt file name %v to %v", fileInfo.Name(), filename)
		} else {
			filename = fileInfo.Name() + o.suffix
		}
		cryptSrcReader, err = cipher.EncryptData(fd)
		if err != nil {
			return fmt.Errorf("encrypt file %v failed: %w", src, err)
		}
		outFile = path.Join(dst, filename)
	} else {
		filename := fileInfo.Name()
		if o.filenameEncryption != "off" {
			filename, err = cipher.DecryptFileName(filename)
			if err != nil {
				return fmt.Errorf("decrypt file name %v failed: %w", src, err)
			}
			log.Infof("decrypt file name %v to %v, ", fileInfo.Name(), filename)
		} else {
			filename = strings.TrimSuffix(filename, o.suffix)
		}

		cryptSrcReader, err = cipher.DecryptData(fd)
		if err != nil {
			return fmt.Errorf("decrypt file %v failed: %w", src, err)
		}
		outFile = path.Join(dst, filename)
	}
	//write new file
	// O_TRUNC：重跑时截断已存在的产物，避免上次较长的输出在末尾留下残字节。
	wr, err := os.OpenFile(outFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create file %v failed: %w", outFile, err)
	}
	defer wr.Close()

	if _, err = io.Copy(wr, cryptSrcReader); err != nil {
		// 读错误（含坏道/CRC）通常在这里暴露：io.Copy 已写出部分字节。
		// 关闭并删除半成品，避免留下看似完整实则损坏的产物。
		_ = wr.Close()
		if rmErr := os.Remove(outFile); rmErr != nil {
			log.Warnf("remove partial output %v failed,err:%v", outFile, rmErr)
		}
		return fmt.Errorf("write file %v failed: %w", outFile, err)
	}
	return nil
}

// check dir exist ,if not ,create
func checkCreateDir(dir string) {
	_, err := os.Stat(dir)

	if os.IsNotExist(err) {
		err := os.MkdirAll(dir, 0755)
		if err != nil {
			log.Fatalf("create dir %v failed,err:%v", dir, err)
		}
		return
	} else if err != nil {
		log.Fatalf("read dir %v err: %v", dir, err)
	}

}

func updateObfusParm(str string) string {
	obfuscatedPrefix := "___Obfuscated___"
	if !strings.HasPrefix(str, obfuscatedPrefix) {
		str, err := obscure.Obscure(str)
		if err != nil {
			log.Fatalf("update obfuscated parameter failed,err:%v", str)
		}
	} else {
		str, _ = strings.CutPrefix(str, obfuscatedPrefix)
	}
	return str
}
