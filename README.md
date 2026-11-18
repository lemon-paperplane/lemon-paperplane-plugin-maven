# lemon-paperplane-plugin-maven

Lemon Paperplane 的 Maven 插件（依赖 java 插件）。

- 识别 `pom.xml`，递归解析 `<modules>`（含 profile 里的模块）并认领；沿 `relativePath` 合并本地父 POM 的属性，`${}` 展开；识别 Spring Boot（父 POM、BOM、依赖、插件）、Kotlin、Java 版本（`maven.compiler.release/target/source`、编译插件配置、`java.version`、Boot 父 POM 默认值）、主类（`start-class`、spring-boot / exec / jar / shade 插件配置）。
- JDK 需求：Maven 运行所需版本（3.9 → 8+，4.x → 17+）与工程目标版本取交集；老目标版本（5/6/7）限定 JDK 上限；Lombok 版本限制。
- 镜像：生成临时全局 settings（`-gs`，`central` 指向镜像并保留 Maven 默认的 HTTP 仓库屏蔽），用户的 `~/.m2/settings.xml` 照常生效；用户已配置镜像时不注入。Maven Wrapper 用 `MVNW_REPOURL` 走镜像。
- 同步：`help:effective-pom`（拿到合并后的有效 POM）+ `dependency:go-offline`（跳过 reactor 兄弟模块）。
- 服务 `prepareClasspath`：`mvn -pl <模块> -am compile dependency:build-classpath`，每个模块各写一份类路径文件；reactor 兄弟模块解析为 `target/classes`。服务 `launchGoals`（spring-boot:run 模式）。
- 运行配置 `maven-goal`；动作：同步、打包、安装到本地仓库、清理。工具链 `maven`（Homebrew、`~/.m2/wrapper/dists`、SDKMAN、`MAVEN_HOME`、PATH；可下载）。
