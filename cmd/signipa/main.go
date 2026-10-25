// Command signipa 用节点同款 go-codesign（signing.SignWithFiles）签一个 ipa。
//
// 为什么不用 `ios sign app` CLI：go-ios 1.3.2 的 CLI 在签名前会先
// GetDeviceWithAddress/ListDevices 找设备，没有 USB 手机就直接失败——
// 但纯签名根本不需要设备。本工具直接调库，零设备依赖。
//
// 用法：
//   go run ./cmd/signipa -in unsigned.ipa -p12 id.p12 -profile p.mobileprovision \
//     -password iPASide -bundleid com.x.y.ZZZ -out signed.ipa
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/danielpaulus/go-ios/ios/signing"
)

func main() {
	in := flag.String("in", "", "input .ipa")
	p12 := flag.String("p12", "", "p12 identity")
	profile := flag.String("profile", "", "mobileprovision")
	password := flag.String("password", "", "p12 password")
	bundleID := flag.String("bundleid", "", "target bundle id")
	out := flag.String("out", "", "output .ipa")
	flag.Parse()

	for name, v := range map[string]string{
		"-in": *in, "-p12": *p12, "-profile": *profile, "-out": *out,
	} {
		if v == "" {
			fmt.Fprintf(os.Stderr, "missing %s\n", name)
			os.Exit(2)
		}
	}

	res, err := signing.SignWithFiles(signing.SignWithFilesOptions{
		AppPath:     *in,
		OutputPath:  *out,
		BundleID:    *bundleID,
		P12Path:     *p12,
		P12Password: *password,
		ProfilePath: *profile,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sign failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("signed: %s\n", res.OutputPath)
}
