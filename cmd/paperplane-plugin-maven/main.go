// paperplane-plugin-maven 是 maven 插件的独立可执行入口。
package main

import (
	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"

	maven "github.com/lemon-paperplane/lemon-paperplane-plugin-maven/plugin"
)

func main() { sdk.Serve(maven.New()) }
