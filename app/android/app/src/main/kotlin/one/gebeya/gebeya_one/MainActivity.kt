package one.gebeya.gebeya_one

import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyPermanentlyInvalidatedException
import android.security.keystore.KeyProperties
import android.util.Base64
import androidx.biometric.BiometricManager
import androidx.biometric.BiometricManager.Authenticators.BIOMETRIC_STRONG
import androidx.biometric.BiometricPrompt
import androidx.core.content.ContextCompat
import io.flutter.embedding.android.FlutterFragmentActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodChannel
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.PrivateKey
import java.security.Signature
import java.security.interfaces.ECPublicKey
import java.security.spec.ECGenParameterSpec

/**
 * Device keys for the API (channel "gebeya/device_keys"). Both are P-256 keys in the Android Keystore,
 * in StrongBox when the phone has it, so the private halves never leave secure hardware.
 *
 * - "g1_dpop" signs a DPoP proof for every API request. It needs no user action.
 * - "g1_bio" signs payment confirmations. It works only right after the phone's strong biometric
 *   check (BiometricPrompt with the signature as its CryptoObject), and stops working for good if
 *   someone adds a fingerprint or face to the phone.
 *
 * FlutterFragmentActivity is required by BiometricPrompt.
 */
class MainActivity : FlutterFragmentActivity() {
    private val dpopAlias = "g1_dpop"
    private val bioAlias = "g1_bio"

    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)
        MethodChannel(flutterEngine.dartExecutor.binaryMessenger, "gebeya/device_keys").setMethodCallHandler { call, result ->
            try {
                when (call.method) {
                    "dpopPublicKey" -> result.success(publicPoint(ensureKey(dpopAlias, biometric = false)))
                    "dpopSign" -> {
                        val data = call.argument<ByteArray>("data")!!
                        val s = Signature.getInstance("SHA256withECDSA")
                        s.initSign(privateKey(dpopAlias) ?: privateKeyAfterCreate(dpopAlias))
                        s.update(data)
                        result.success(s.sign())
                    }
                    "dpopReset" -> { deleteKey(dpopAlias); result.success(null) }
                    "biometricAvailable" -> result.success(
                        BiometricManager.from(this).canAuthenticate(BIOMETRIC_STRONG) == BiometricManager.BIOMETRIC_SUCCESS
                    )
                    "biometricCreate" -> {
                        deleteKey(bioAlias)
                        val pub = ensureKey(bioAlias, biometric = true)
                        result.success(Base64.encodeToString(pub.encoded, Base64.NO_WRAP)) // SPKI DER
                    }
                    "biometricDelete" -> { deleteKey(bioAlias); result.success(null) }
                    "biometricEnrolled" -> result.success(keyStore().containsAlias(bioAlias))
                    "biometricSign" -> biometricSign(
                        call.argument<ByteArray>("data")!!,
                        call.argument<String>("title") ?: "",
                        call.argument<String>("subtitle") ?: "",
                        call.argument<String>("cancel") ?: "Cancel",
                        result,
                    )
                    else -> result.notImplemented()
                }
            } catch (e: Exception) {
                result.error("key_error", e.javaClass.simpleName + ": " + (e.message ?: ""), null)
            }
        }
    }

    private fun keyStore(): KeyStore = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }

    private fun privateKey(alias: String): PrivateKey? = keyStore().getKey(alias, null) as PrivateKey?

    private fun privateKeyAfterCreate(alias: String): PrivateKey {
        ensureKey(alias, biometric = false)
        return privateKey(alias)!!
    }

    private fun deleteKey(alias: String) {
        val ks = keyStore()
        if (ks.containsAlias(alias)) ks.deleteEntry(alias)
    }

    /** Returns the existing public key for [alias], or creates the key pair. */
    private fun ensureKey(alias: String, biometric: Boolean): ECPublicKey {
        keyStore().getCertificate(alias)?.let { return it.publicKey as ECPublicKey }
        fun spec(strongBox: Boolean): KeyGenParameterSpec {
            val b = KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_SIGN)
                .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
                .setDigests(KeyProperties.DIGEST_SHA256)
            if (biometric) {
                b.setUserAuthenticationRequired(true)
                b.setInvalidatedByBiometricEnrollment(true)
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                    b.setUserAuthenticationParameters(0, KeyProperties.AUTH_BIOMETRIC_STRONG) // every use
                }
            }
            if (strongBox && Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) b.setIsStrongBoxBacked(true)
            return b.build()
        }
        val gen = KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, "AndroidKeyStore")
        val pair = try {
            gen.initialize(spec(strongBox = true)); gen.generateKeyPair()
        } catch (e: Exception) {
            // No StrongBox on this phone (most phones; some vendors throw ProviderException rather than
            // StrongBoxUnavailableException). The TEE-backed keystore is still secure hardware.
            gen.initialize(spec(strongBox = false)); gen.generateKeyPair()
        }
        return pair.public as ECPublicKey
    }

    /** The public point as base64url x and y (32 bytes each), for the DPoP JWK. */
    private fun publicPoint(k: ECPublicKey): Map<String, String> {
        fun fixed(b: java.math.BigInteger): String {
            val raw = b.toByteArray()
            val out = ByteArray(32)
            val src = if (raw.size > 32) raw.copyOfRange(raw.size - 32, raw.size) else raw
            System.arraycopy(src, 0, out, 32 - src.size, src.size)
            return Base64.encodeToString(out, Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP)
        }
        return mapOf("x" to fixed(k.w.affineX), "y" to fixed(k.w.affineY))
    }

    private fun biometricSign(data: ByteArray, title: String, subtitle: String, cancel: String, result: MethodChannel.Result) {
        val key = privateKey(bioAlias)
        if (key == null) {
            result.error("not_enrolled", "no biometric key on this device", null); return
        }
        val sig = Signature.getInstance("SHA256withECDSA")
        try {
            sig.initSign(key)
        } catch (e: KeyPermanentlyInvalidatedException) {
            deleteKey(bioAlias) // a fingerprint or face was added: the user must set it up again with the PIN
            result.error("not_enrolled", "biometric key invalidated", null); return
        }
        var replied = false
        fun reply(f: () -> Unit) { if (!replied) { replied = true; f() } }
        val prompt = BiometricPrompt(this, ContextCompat.getMainExecutor(this), object : BiometricPrompt.AuthenticationCallback() {
            override fun onAuthenticationSucceeded(r: BiometricPrompt.AuthenticationResult) {
                try {
                    val s = r.cryptoObject!!.signature!!
                    s.update(data)
                    val der = s.sign()
                    reply { result.success(Base64.encodeToString(der, Base64.NO_WRAP)) }
                } catch (e: Exception) {
                    reply { result.error("key_error", e.message, null) }
                }
            }

            override fun onAuthenticationError(code: Int, msg: CharSequence) {
                val c = when (code) {
                    BiometricPrompt.ERROR_USER_CANCELED, BiometricPrompt.ERROR_NEGATIVE_BUTTON, BiometricPrompt.ERROR_CANCELED -> "cancelled"
                    BiometricPrompt.ERROR_LOCKOUT, BiometricPrompt.ERROR_LOCKOUT_PERMANENT -> "locked_out"
                    else -> "unavailable"
                }
                reply { result.error(c, msg.toString(), null) }
            }
            // onAuthenticationFailed (a finger that didn't match) keeps the prompt open; nothing to do.
        })
        val info = BiometricPrompt.PromptInfo.Builder()
            .setTitle(title)
            .setSubtitle(subtitle)
            .setNegativeButtonText(cancel)
            .setAllowedAuthenticators(BIOMETRIC_STRONG)
            .setConfirmationRequired(true)
            .build()
        prompt.authenticate(info, BiometricPrompt.CryptoObject(sig))
    }
}
