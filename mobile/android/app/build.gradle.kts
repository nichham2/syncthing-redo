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
        versionCode = 1
        versionName = "0.1.0"
        resValue("string", "app_name", appName)
        ndk {
            // The Go bridge is built for these ABIs (see mobile/build-bridge.sh).
            abiFilters += listOf("arm64-v8a", "x86_64")
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            // Sign release builds with your own key; debug builds are
            // signed with the debug key and can be sideloaded directly.
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
