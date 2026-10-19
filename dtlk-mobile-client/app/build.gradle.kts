plugins {
    id("com.android.application")
}

// Same format as the CLI: yymmdd (commit date, KST)-<short sha>
val gitVersion: String = providers.exec {
    commandLine("git", "log", "-1", "--date=format-local:%y%m%d", "--format=%cd-%h", "--abbrev=7")
    environment("TZ", "Asia/Seoul")
    isIgnoreExitValue = true
}.standardOutput.asText.get().trim().ifEmpty { "dev" }

android {
    namespace = "com.ga111o.dtlk"
    compileSdk = 36

    defaultConfig {
        applicationId = "com.ga111o.dtlk"
        minSdk = 26
        targetSdk = 36
        versionCode = 1
        versionName = gitVersion
    }

    buildFeatures {
        buildConfig = true
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

dependencies {
    // 1.5.7-13 and later require compileSdk 37
    implementation("com.github.luben:zstd-jni:1.5.7-12@aar")

    // the jar carries the desktop native library the JVM tests need
    testImplementation("com.github.luben:zstd-jni:1.5.7-12")
    testImplementation("junit:junit:4.13.2")
    testImplementation("org.json:json:20260814")
    testImplementation("org.apache.commons:commons-compress:1.28.0")
}
