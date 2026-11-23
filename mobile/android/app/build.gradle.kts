plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

val appName = providers.gradleProperty("keepsake.appName").get()
val appId = providers.gradleProperty("keepsake.applicationId").get()

android {
    namespace = "app.keepsake"
    compileSdk = 35

    defaultConfig {
        applicationId = appId
        minSdk = 29 // Android 10: scoped storage APIs used by MediaStoreBridge
        targetSdk = 35
        // Increases with every CI run so updates install over the old app.
        versionCode = (System.getenv("GITHUB_RUN_NUMBER") ?: "1").toInt()
        versionName = "0.1.0"
        resValue("string", "app_name", appName)
        ndk {
            // The Go bridge is built for these ABIs (see mobile/build-bridge.sh).
            abiFilters += listOf("arm64-v8a", "x86_64")
        }
    }

    // Release builds are signed with your own key, from these environment
    // variables (CI sets them from the ANDROID_* secrets). Keep the same key
    // forever: Android only installs an update signed with the same key, and
    // uninstalling to switch keys wipes the app's sync state.
    val keystorePath = System.getenv("KEYSTORE_PATH")
    signingConfigs {
        if (keystorePath != null) {
            create("release") {
                storeFile = file(keystorePath)
                storePassword = System.getenv("KEYSTORE_PASSWORD")
                keyAlias = System.getenv("KEY_ALIAS")
                keyPassword = System.getenv("KEY_PASSWORD")
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            isDebuggable = false
            if (keystorePath != null) signingConfig = signingConfigs.getByName("release")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
    buildFeatures {
        resValues = true
    }
}

dependencies {
    // Built by mobile/build-bridge.sh with gomobile.
    implementation(files("libs/keepsake-core.aar"))

    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.activity:activity-ktx:1.9.3")
    implementation("androidx.lifecycle:lifecycle-runtime-ktx:2.8.7")
    implementation("androidx.work:work-runtime-ktx:2.9.1")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.8.1")
}
