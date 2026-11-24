package io.github.pgodlews.heymuse

import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.provider.Settings as AndroidSettings
import android.util.Log
import heymuse.App
import heymuse.Ears
import heymuse.Heymuse
import heymuse.Host
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.concurrent.thread

/** Where a turn stands. */
enum class Phase { IDLE, LISTENING, THINKING, SPEAKING }

/** What the Go core says about the Portal and Muse (heymuse.Status). */
data class MuseStatus(
    val phase: String = Heymuse.PhaseStopped,
    val username: String = "",
    /** What the owner's Muse is called. */
    val agentName: String = "",
    val bleName: String = "",
    val error: String = "",
    val hasSdkToken: Boolean = false,
    val pairingState: String = "",
    val pairingReason: String = "",
    val pairingError: String = "",
    val pairingDone: Boolean = false,
    val pairingFailed: Boolean = false,
) {
    val online get() = phase == Heymuse.PhaseOnline
    val pairing get() = phase == Heymuse.PhasePairing
    // The Muse app does not always send a user name, so being connected counts as paired too.
    val paired get() = username.isNotEmpty() ||
        phase == Heymuse.PhaseOnline || phase == Heymuse.PhaseConnecting || phase == Heymuse.PhaseOffline

    companion object {
        fun parse(json: String): MuseStatus = runCatching {
            val o = JSONObject(json)
            val p = o.optJSONObject("pairing")
            MuseStatus(
                phase = o.optString("phase"),
                username = o.optString("username"),
                agentName = o.optString("agentName"),
                bleName = o.optString("bleName"),
                error = o.optString("error"),
                hasSdkToken = o.optBoolean("hasSdkToken"),
                pairingState = p?.optString("state") ?: "",
                pairingReason = p?.optString("reason") ?: "",
                pairingError = p?.optString("error") ?: "",
                pairingDone = p?.optBoolean("done") ?: false,
                pairingFailed = p?.optBoolean("failed") ?: false,
            )
        }.getOrDefault(MuseStatus())
    }
}

data class TimerItem(val id: Long, val label: String, val endMs: Long)
data class AlarmItem(val id: Long, val hour: Int, val minute: Int, val days: String, val label: String, val on: Boolean, val nextMs: Long)

data class UiState(
    val phase: Phase = Phase.IDLE,
    /** What Muse heard of the last question. */
    val heard: String = "",
    /** Muse's last answer, or why there was none. */
    val answer: String = "",
    val answerIsProblem: Boolean = false,
    /** How loud the mic (listening) or the voice (speaking) is, 0 to 1. */
    val level: Float = 0f,
    val status: MuseStatus = MuseStatus(),
    val mic: Mic.State = Mic.State.OFF,
    /** What is ringing, like "Timer" or "Alarm: gym", or null. */
    val ringing: String? = null,
    val timers: List<TimerItem> = emptyList(),
    val alarms: List<AlarmItem> = emptyList(),
    /** Why pairing could not start, or the wake word could not load. */
    val problem: String = "",
    val wakeReady: Boolean = false,
    /** A live call with Muse is on. */
    val inCall: Boolean = false,
    /** The call is a video call: Muse's picture is on screen once it comes. */
    val videoCall: Boolean = false,
    /** The size of Muse's picture, once known. */
    val videoWidth: Int = 0,
    val videoHeight: Int = 0,
    /** Bumped when the owner's avatar files change. */
    val avatarVersion: Int = 0,
)

/**
 * The Portal as Muse's voice: the one owner of the Go core, the microphone, the voice and the
 * ring. The service starts it and the screen watches [state].
 *
 * Turns are driven on the main thread. The microphone thread only feeds the ears and posts what
 * they decide; the Go core's callbacks are posted the same way, so none of them ever waits on the
 * core while the core waits on it.
 */
object Assistant {
    private const val TAG = "HeyMuse"
    /** The loud start of the chime, in bytes of 16 kHz audio: 300 ms. */
    private const val CHIME_SKIP = 16000 * 2 * 3 / 10
    /** After Muse would not take a call, how long to ask with voice notes before trying again. */
    private const val CALL_RETRY_MS = 10 * 60_000L
    private const val AVATAR_EVERY_MS = 6 * 3600_000L
    private const val SILENCE_SHOWN_MS = 2500L
    private const val VIDEO_IDLE_MS = 45_000L
    private val main = Handler(Looper.getMainLooper())

    private val _state = MutableStateFlow(UiState())
    val state: StateFlow<UiState> = _state.asStateFlow()

    lateinit var settings: Settings
        private set
    private lateinit var context: Context
    private var app: App? = null
    private var mic: Mic? = null
    private var voice: Voice? = null
    private var ringer: Ringer? = null
    private var peripheral: BlePeripheral? = null

    /**
     * The turn whose answer is awaited, from heymuse; anything about another is old news. Its number
     * may arrive after its first event, so any turn from [minTurn] on is taken as the one.
     */
    private var turn = 0L
    private var minTurn = 1L
    private var lastTurn = 0L

    // The microphone thread's own.
    private var ears: Ears? = null
    @Volatile private var earsWanted: Float = -1f
    private val beginUtterance = AtomicBoolean(false)
    private val finishNow = AtomicBoolean(false)
    private val resetWake = AtomicBoolean(false)
    /** Audio of the utterance still to drop: the chime, heard through the microphone. */
    private var skip = 0
    @Volatile private var phase = Phase.IDLE
    private var lastLevelAt = 0L

    // The live call. callId is 0 until StartCall says which; [calling] is set from the wake on.
    @Volatile private var calling = false
    private var callId = 0L
    @Volatile private var callPlayer: CallPlayer? = null
    @Volatile private var callHeld = false
    /** Audio still to drop at the start of a call: the chime. */
    private var callSkip = 0
    /** When Muse last would not take a call: the old way is used for a while after. */
    private var callDeclinedAt = 0L
    private var answerPlayer: android.media.MediaPlayer? = null

    // A video call's codecs. The decoders are fed from the core's goroutines, the encoder from the mic.
    @Volatile private var callVideo = false
    @Volatile private var opusIn: Opus.Decoder? = null
    @Volatile private var opusOut: Opus.Encoder? = null
    @Volatile private var picture: VideoDecoder? = null
    private val up48 = Upsample16to48()
    @Volatile private var lastCallVoice = 0L
    /** When Muse last would not take a video call: voice-only calls for a while after. */
    private var videoDeclinedAt = 0L
    private var avatarSyncedAt = 0L

    /** Whether the screen is showing, so a wake word knows whether to bring it up. */
    @Volatile var visible = false

    private var started = false

    fun start(ctx: Context) {
        if (started) return
        started = true
        context = ctx.applicationContext
        AudioFocus.init(context)
        settings = Settings(context)
        voice = Voice(context, settings)
        ringer = Ringer(context).also { r -> r.onEnded = { main.post { _state.update { it.copy(ringing = null) } } } }

        val dir = File(context.filesDir, "muse").apply { mkdirs() }
        importPairing(dir)
        val core = Heymuse.newApp(dir.absolutePath, settings.displayName, BuildConfig.VERSION_NAME, host)
        core.setSDKToken(settings.sdkToken)
        core.setMuseVoice(settings.museVoice)
        core.start()
        app = core
        refreshDevice()

        earsWanted = settings.sensitivity
        mic = Mic(context, { settings.voiceProcessing }, ::micState, ::onAudio).also { it.start() }
    }

    /**
     * Takes a pairing made elsewhere (hey-muse's musepair, on a computer with Bluetooth), pushed to
     * the app's external files as muse.json: for a Portal whose own Bluetooth cannot advertise. The
     * copy there is deleted once taken, since anything in that folder is readable over adb.
     */
    private fun importPairing(dir: File) {
        val pushed = context.getExternalFilesDir(null)?.let { File(it, "muse.json") } ?: return
        if (!pushed.isFile) return
        try {
            val st = JSONObject(pushed.readText())
            require(st.optJSONObject("credentials")?.optString("refresh_token")?.isNotEmpty() == true) { "it holds no pairing" }
            val tmp = File(dir, "muse.json.tmp")
            tmp.writeText(st.toString())
            if (!tmp.renameTo(File(dir, "muse.json"))) throw IllegalStateException("could not keep it")
            Log.i(TAG, "took the pairing from ${pushed.path}")
        } catch (e: Exception) {
            Log.w(TAG, "could not take the pairing from ${pushed.path}: ${e.message}")
        } finally {
            pushed.delete()
        }
    }

    fun stop() {
        if (!started) return
        started = false
        mic?.stop()
        voice?.stop()
        stopAnswerAudio()
        endCallHere()
        app?.hangUp()
        ringer?.stop()
        peripheral = null
        app?.stop()
        app = null
        phase = Phase.IDLE
        _state.update { it.copy(phase = Phase.IDLE, level = 0f) }
    }

    /**
     * Debug: asks Muse for a reply in its own voice and plays the MP3 Muse sends, and with [call]
     * asks whether Muse admits a realtime call. The result goes to logcat under HeyMuseProbe.
     */
    fun probeVoice(call: Boolean) {
        val core = app ?: run { Log.w("HeyMuseProbe", "the core is not running"); return }
        thread(name = "muse-voice-probe") {
            Log.i("HeyMuseProbe", "probing Muse's voice (call=$call)")
            val report = core.probeVoice(context.filesDir.path, call)
            Log.i("HeyMuseProbe", report)
            val mp3 = File(context.filesDir, "muse-voice.mp3")
            if (!JSONObject(report).optString("tts_file").isNullOrEmpty() && mp3.length() > 0) {
                runCatching {
                    android.media.MediaPlayer().apply {
                        setAudioAttributes(
                            android.media.AudioAttributes.Builder()
                                .setUsage(android.media.AudioAttributes.USAGE_MEDIA)
                                .setContentType(android.media.AudioAttributes.CONTENT_TYPE_SPEECH)
                                .build()
                        )
                        setDataSource(mp3.path)
                        setOnCompletionListener { it.release(); Log.i("HeyMuseProbe", "played Muse's voice") }
                        prepare()
                        start()
                    }
                }.onFailure { Log.w("HeyMuseProbe", "could not play the MP3: $it") }
            }
        }
    }

    /** Fetches the owner's own Muse avatar (still and phone clips) into files/avatar. */
    fun syncAvatar() {
        val core = app ?: return
        thread(name = "muse-avatar-sync") {
            val dir = File(context.filesDir, "avatar")
            val manifest = core.syncAvatar(dir.path)
            if (manifest.isEmpty()) {
                Log.w(TAG, "the avatar could not be fetched")
                return@thread
            }
            val m = JSONObject(manifest)
            Log.i(TAG, "avatar: still=${m.optString("still").isNotEmpty()} clips=${m.optJSONObject("clips")?.keys()?.asSequence()?.toList()}")
            main.post { _state.update { it.copy(avatarVersion = it.avatarVersion + 1) } }
        }
    }

    /** Re-reads the settings after the screen changed them. */
    fun settingsChanged(micChanged: Boolean = false) {
        val core = app ?: return
        core.setSDKToken(settings.sdkToken)
        core.setDisplayName(settings.displayName)
        core.setMuseVoice(settings.museVoice)
        earsWanted = settings.sensitivity
        if (micChanged) thread { mic?.restart() }
    }

    // ---- Turns, on the main thread ----

    /** Cuts Muse off in a voice call; the call goes on. */
    fun interrupt() {
        if (!calling) return
        callPlayer?.flush()
        thread { app?.interruptCall() }
    }

    /** Ends the call, if there is one. */
    fun hangUp() {
        if (!calling) return
        thread { app?.hangUp() }
        if (callId == 0L) endCall()
    }

    /** A tap on the screen: whatever moves the turn on. */
    fun tap() {
        when {
            ringer?.ringing == true -> stopRinging()
            calling -> {
                val p = callPlayer
                if (!callVideo && p != null && p.playing()) {
                    // Cut Muse off; the call goes on.
                    p.flush()
                    thread { app?.interruptCall() }
                } else {
                    thread { app?.hangUp() }
                    if (callId == 0L) endCall()
                }
            }
            phase == Phase.IDLE -> listen()
            phase == Phase.LISTENING -> finishNow.set(true)
            phase == Phase.THINKING -> {
                thread { app?.cancelTurn() }
                toIdle()
            }
            phase == Phase.SPEAKING -> {
                voice?.stop()
                stopAnswerAudio()
                toIdle()
            }
        }
    }

    /** "Hey Muse" or a tap: a live call when Muse takes calls, and otherwise a voice note. */
    private fun listen() {
        val status = _state.value.status
        val declinedLately = System.currentTimeMillis() - callDeclinedAt < CALL_RETRY_MS
        if (settings.conversation && status.online && !declinedLately) startCall() else listenForNote()
    }

    private fun startCall() {
        val core = app ?: return listenForNote()
        val video = settings.videoCall && System.currentTimeMillis() - videoDeclinedAt > CALL_RETRY_MS
        voice?.stop()
        stopAnswerAudio()
        AudioFocus.request()
        Sound.play(context, R.raw.wake_chime)
        newTurn()
        callId = 0
        callHeld = false
        callSkip = CHIME_SKIP
        callVideo = video
        lastCallVoice = System.currentTimeMillis()
        if (video) {
            val player = CallPlayer(Opus.RATE, ::callLevel)
            callPlayer = player
            val ok = runCatching {
                opusIn = Opus.Decoder { pcm -> player.write(pcm) }
                opusOut = Opus.Encoder { packet -> app?.callOpus(packet) }
            }
            if (ok.isFailure) {
                Log.w(TAG, "no Opus codec, so a voice-only call", ok.exceptionOrNull())
                closeVideoMedia()
                callVideo = false
            }
        }
        if (!callVideo) callPlayer = CallPlayer(Heymuse.CallRate.toInt(), ::callLevel)
        calling = true
        phase = Phase.LISTENING
        _state.update { it.copy(phase = Phase.LISTENING, inCall = true, videoCall = callVideo, videoWidth = 0, videoHeight = 0,
            heard = "", answer = "", answerIsProblem = false, level = 0f) }
        main.removeCallbacks(callTick)
        main.postDelayed(callTick, 200)
        val asVideo = callVideo
        thread { val id = if (asVideo) core.startVideoCall() else core.startCall(); main.post { if (calling && callId == 0L) callId = id } }
    }

    /** Whether a call event belongs to the call on now. */
    private fun ourCall(id: Long): Boolean {
        if (!calling) return false
        if (callId == 0L) callId = id
        return callId == id
    }

    /** The screen's surface for Muse's picture, while a video call can show it. */
    fun attachVideo(surface: android.view.Surface?) {
        picture?.close()
        picture = null
        if (surface == null || !callVideo) return
        picture = VideoDecoder(surface,
            askKeyframe = { thread { app?.callKeyframe() } },
            size = { w, h -> main.post { _state.update { it.copy(videoWidth = w, videoHeight = h) } } })
        thread { app?.callKeyframe() }
    }

    private fun closeVideoMedia() {
        opusIn?.close(); opusIn = null
        opusOut?.close(); opusOut = null
        picture?.close(); picture = null
    }

    /** Follows Muse speaking and stopping, for the screen. */
    private val callTick: Runnable = object : Runnable {
        override fun run() {
            if (!calling) return
            if (callPlayer?.playing() == true) lastCallVoice = System.currentTimeMillis()
            // A video call has no idle limit of its own on Muse's side: nobody talking for a while ends it.
            if (callVideo && System.currentTimeMillis() - lastCallVoice > VIDEO_IDLE_MS) {
                Log.i(TAG, "nobody has spoken for a while, so the video call ends")
                thread { app?.hangUp() }
                lastCallVoice = System.currentTimeMillis()
            }
            val speaking = callPlayer?.playing() == true
            val want = if (speaking) Phase.SPEAKING else Phase.LISTENING
            if (phase != want) {
                phase = want
                _state.update { it.copy(phase = want, level = 0f) }
            }
            main.postDelayed(this, 150)
        }
    }

    private fun callLevel(level: Float) {
        main.post { if (calling && phase == Phase.SPEAKING) _state.update { it.copy(level = level) } }
    }

    /** Puts the call away here; the core has ended it, or never started it. */
    private fun endCallHere() {
        calling = false
        closeVideoMedia()
        callVideo = false
        main.removeCallbacks(callTick)
        callPlayer?.stop()
        callPlayer = null
        callId = 0
        callHeld = false
    }

    private fun endCall() {
        endCallHere()
        _state.update { it.copy(inCall = false) }
        toIdle()
    }

    private fun listenForNote() {
        val status = _state.value.status
        if (!status.online) {
            problem(excuse(status))
            return
        }
        voice?.stop()
        AudioFocus.request()
        Sound.play(context, R.raw.wake_chime)
        newTurn()
        phase = Phase.LISTENING
        finishNow.set(false)
        beginUtterance.set(true)
        _state.update { it.copy(phase = Phase.LISTENING, heard = "", answer = "", answerIsProblem = false, level = 0f) }
    }

    private fun excuse(s: MuseStatus): String = when {
        s.pairing -> "I'm pairing with the Muse app right now."
        !s.paired -> "This Portal is not paired with Muse yet. Open the settings to pair it."
        else -> "I can't reach Muse right now."
    }

    private fun onUtterance(pcm: ByteArray) {
        if (phase != Phase.LISTENING) return
        phase = Phase.THINKING
        _state.update { it.copy(phase = Phase.THINKING, level = 0f) }
        val core = app ?: return toIdle()
        thread { val id = core.askVoice(pcm); main.post { asked(id) } }
    }

    private fun nothingSaid() {
        if (phase == Phase.LISTENING) toIdle()
    }

    private fun toIdle() {
        phase = Phase.IDLE
        resetWake.set(true)
        if (ringer?.ringing != true) {
            AudioFocus.abandon()
        }
        _state.update { it.copy(phase = Phase.IDLE, level = 0f) }
    }

    /** Says why something cannot be done, and shows it. */
    private fun problem(text: String) {
        phase = Phase.SPEAKING
        AudioFocus.request()
        _state.update { it.copy(phase = Phase.SPEAKING, answer = text, answerIsProblem = true, heard = "") }
        say(text, again = false)
    }

    private fun say(text: String, again: Boolean) {
        val v = voice ?: return toIdle()
        v.say(text, level = ::voiceLevel) { finished ->
            main.post {
                if (phase != Phase.SPEAKING) return@post
                // An answer that asks something gets its answer without the wake word.
                if (finished && again && text.trimEnd().endsWith("?")) listen() else toIdle()
            }
        }
    }

    private fun voiceLevel(level: Float) {
        val now = System.currentTimeMillis()
        if (now - lastLevelAt < 40 && level > 0f) return
        lastLevelAt = now
        main.post { if (phase == Phase.SPEAKING) _state.update { it.copy(level = level) } }
    }

    private fun newTurn() {
        turn = 0
        minTurn = lastTurn + 1
    }

    private fun asked(id: Long) {
        lastTurn = maxOf(lastTurn, id)
        if (phase == Phase.THINKING && turn == 0L && id >= minTurn) turn = id
    }

    /** Whether a turn event belongs to the turn awaited. */
    private fun ours(id: Long): Boolean {
        if (phase != Phase.THINKING || id < minTurn) return false
        lastTurn = maxOf(lastTurn, id)
        if (turn == 0L) turn = id
        return turn == id
    }

    // ---- The microphone thread ----

    private fun onAudio(buf: ByteArray, n: Int) {
        if (calling) {
            val pcm = buf.copyOf(n)
            if (callSkip > 0) {
                callSkip -= n
                return
            }
            val p = callPlayer
            // Half duplex: while Muse can be heard in the room, the room is not sent back to it.
            val gated = callHeld || (p != null && p.audible())
            if (callVideo) {
                // A video call's media keeps its pace: the room's echo goes as silence, not as nothing.
                val enc = opusOut ?: return
                val frame = if (gated) ByteArray(n) else pcm
                if (!gated && Voice.loudness(pcm, n) > 0.05f) lastCallVoice = System.currentTimeMillis()
                enc.feed(up48.feed(frame, frame.size))
                if (!gated) micLevel(pcm)
                return
            }
            if (gated) return
            app?.callAudio(pcm)
            micLevel(pcm)
            return
        }
        val e = earsFor() ?: return
        val pcm = buf.copyOf(n)
        if (resetWake.getAndSet(false)) e.resetWake()
        when (phase) {
            Phase.LISTENING -> {
                if (beginUtterance.getAndSet(false)) {
                    e.begin()
                    skip = CHIME_SKIP
                }
                if (skip > 0 && !finishNow.get()) {
                    skip -= pcm.size
                    return
                }
                val result = if (finishNow.getAndSet(false)) Heymuse.Finished else e.listen(pcm)
                micLevel(pcm)
                when (result) {
                    Heymuse.Finished -> {
                        val utterance = e.utterance()
                        main.post { onUtterance(utterance) }
                    }
                    Heymuse.NothingSaid -> main.post { nothingSaid() }
                }
            }
            Phase.IDLE -> {
                if (settings.wakeWord && e.wake(pcm)) main.post { onWake() }
            }
            // While Muse thinks the room is not listened to; while the Portal speaks, its own voice
            // would be heard. A ring can be stopped by the wake word, as on the Echo.
            Phase.THINKING, Phase.SPEAKING -> {
                if (ringer?.ringing == true && settings.wakeWord && e.wake(pcm)) main.post { onWake() }
            }
        }
    }

    private fun earsFor(): Ears? {
        val want = earsWanted
        if (ears != null && want < 0) return ears
        if (want >= 0) {
            earsWanted = -1f
            ears = try {
                Heymuse.newEars(File(context.filesDir, "wake").absolutePath, want.toDouble()).also {
                    main.post { _state.update { s -> s.copy(wakeReady = true) } }
                }
            } catch (e: Exception) {
                Log.e(TAG, "the wake word did not load", e)
                main.post { _state.update { s -> s.copy(wakeReady = false, problem = "The wake word did not load: ${e.message}") } }
                null
            }
        }
        return ears
    }

    private fun micLevel(pcm: ByteArray) {
        val now = System.currentTimeMillis()
        if (now - lastLevelAt < 40) return
        lastLevelAt = now
        val level = Voice.loudness(pcm, pcm.size)
        main.post { if (phase == Phase.LISTENING) _state.update { it.copy(level = level) } }
    }

    /**
     * The microphone's state, for the screen. While the Portal itself is speaking, its echo
     * cancellation hands back exact silence, which looks like a muted microphone; so silence counts
     * only when it lasts [SILENCE_SHOWN_MS] with nothing being played.
     */
    private fun micState(s: Mic.State) {
        main.post {
            main.removeCallbacks(showSilenced)
            if (s == Mic.State.SILENCED) main.postDelayed(showSilenced, SILENCE_SHOWN_MS)
            else _state.update { it.copy(mic = s) }
        }
    }

    private val showSilenced: Runnable = object : Runnable {
        override fun run() {
            val speaking = phase == Phase.SPEAKING || callPlayer?.playing() == true || answerPlayer != null
            if (speaking) main.postDelayed(this, SILENCE_SHOWN_MS)
            else if (mic?.state == Mic.State.SILENCED) _state.update { it.copy(mic = Mic.State.SILENCED) }
        }
    }

    // ---- Muse's own voice outside a call ----

    /** Plays an answer in Muse's own voice, or says it in the Portal's voice if it will not play. */
    private fun playAnswer(text: String, mp3: ByteArray) {
        stopAnswerAudio()
        val file = File(context.cacheDir, "answer.mp3")
        val ok = runCatching {
            file.writeBytes(mp3)
            answerPlayer = android.media.MediaPlayer().apply {
                setAudioAttributes(
                    android.media.AudioAttributes.Builder()
                        .setUsage(android.media.AudioAttributes.USAGE_MEDIA)
                        .setContentType(android.media.AudioAttributes.CONTENT_TYPE_SPEECH)
                        .build()
                )
                setDataSource(file.path)
                setOnCompletionListener { mp ->
                    mp.release()
                    if (answerPlayer === mp) answerPlayer = null
                    main.post {
                        if (phase != Phase.SPEAKING) return@post
                        if (text.trimEnd().endsWith("?")) listen() else toIdle()
                    }
                }
                prepare()
                start()
            }
        }
        if (ok.isFailure) {
            Log.w(TAG, "Muse's voice did not play, so the Portal says it", ok.exceptionOrNull())
            say(text, again = true)
        }
    }

    private fun stopAnswerAudio() {
        answerPlayer?.let { runCatching { it.stop() }; it.release() }
        answerPlayer = null
    }

    private fun onWake() {
        if (ringer?.ringing == true) {
            stopRinging()
            return
        }
        if (phase != Phase.IDLE) return
        Log.i(TAG, "wake word")
        bringToFront()
        listen()
    }

    private fun bringToFront() {
        if (!settings.openOnWake) return
        val canStart = Build.VERSION.SDK_INT < Build.VERSION_CODES.Q || AndroidSettings.canDrawOverlays(context)
        if (!canStart) {
            Log.w(TAG, "cannot bring to front: missing overlay permission")
            return
        }
        runCatching {
            context.startActivity(Intent(context, MainActivity::class.java).addFlags(
                Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_REORDER_TO_FRONT
            ))
        }.onFailure { Log.w(TAG, "could not come to the front", it) }
    }

    fun stopRinging() {
        ringer?.stop()
        if (phase == Phase.IDLE) AudioFocus.abandon()
        _state.update { it.copy(ringing = null) }
    }

    // ---- Pairing, timers and alarms, for the screen ----

    fun startPairing() {
        val core = app ?: return
        _state.update { it.copy(problem = "") }
        val p = BlePeripheral(context, onWrite = { core.pairingWrite(it) }, onDisconnect = { core.pairingDisconnected() })
        peripheral = p
        thread(name = "pairing") {
            try {
                core.startPairing(p, "")
            } catch (e: Exception) {
                Log.w(TAG, "pairing did not start", e)
                main.post { _state.update { it.copy(problem = e.message ?: "Pairing did not start.") } }
            }
        }
    }

    fun cancelPairing() {
        thread { app?.cancelPairing() }
    }

    fun unpair() {
        thread {
            runCatching { app?.unpair() }.onFailure { e -> main.post { _state.update { it.copy(problem = e.message ?: "") } } }
        }
    }

    fun cancelTimer(id: Long) = thread { app?.cancelTimer(id) }
    fun deleteAlarm(id: Long) = thread { runCatching { app?.deleteAlarm(id) } }

    /** Asks Muse in words, from the screen. */
    fun ask(text: String) {
        if (text.isBlank()) return
        val status = _state.value.status
        if (!status.online) return problem(excuse(status))
        voice?.stop()
        newTurn()
        phase = Phase.THINKING
        _state.update { it.copy(phase = Phase.THINKING, heard = text, answer = "", answerIsProblem = false) }
        val core = app ?: return toIdle()
        thread { val id = core.askText(text); main.post { asked(id) } }
    }

    /** Says a line in the chosen voice, to hear what it sounds like. */
    fun testVoice() {
        if (phase != Phase.IDLE) return
        phase = Phase.SPEAKING
        _state.update { it.copy(phase = Phase.SPEAKING) }
        say("Hi, I'm Muse. This is how I sound on your Portal.", again = false)
    }

    private fun refreshDevice() {
        val core = app ?: return
        val timers = runCatching {
            val a = JSONArray(core.timers())
            (0 until a.length()).map { i -> a.getJSONObject(i).let { TimerItem(it.getLong("id"), it.optString("label"), it.getLong("end")) } }
        }.getOrDefault(emptyList())
        val alarms = runCatching {
            val a = JSONArray(core.alarms())
            (0 until a.length()).map { i ->
                a.getJSONObject(i).let {
                    AlarmItem(it.getLong("id"), it.getInt("hour"), it.getInt("minute"), it.optString("days"),
                        it.optString("label"), it.optBoolean("on"), it.optLong("next"))
                }
            }
        }.getOrDefault(emptyList())
        main.post { _state.update { it.copy(timers = timers, alarms = alarms) } }
    }

    // ---- The Go core's callbacks, on its goroutines ----

    private val host = object : Host {
        override fun onStatus(status: String) {
            val s = MuseStatus.parse(status)
            main.post {
                _state.update { it.copy(status = s) }
                // The owner's avatar: fetched once online, and again every few hours.
                if (s.online && settings.myMuse && System.currentTimeMillis() - avatarSyncedAt > AVATAR_EVERY_MS) {
                    avatarSyncedAt = System.currentTimeMillis()
                    syncAvatar()
                }
            }
        }

        override fun onHeard(turn: Long, text: String) {
            main.post { if (ours(turn)) _state.update { it.copy(heard = text) } }
        }

        override fun onAnswer(turn: Long, text: String) {
            main.post {
                if (!ours(turn)) return@post
                phase = Phase.SPEAKING
                _state.update { it.copy(phase = Phase.SPEAKING, answer = text, answerIsProblem = false) }
                say(text, again = true)
            }
        }

        override fun onAnswerAudio(turn: Long, text: String, mp3: ByteArray) {
            main.post {
                if (!ours(turn)) return@post
                phase = Phase.SPEAKING
                _state.update { it.copy(phase = Phase.SPEAKING, answer = text, answerIsProblem = false) }
                playAnswer(text, mp3)
            }
        }

        override fun onCallReady(call: Long) {
            main.post { if (ourCall(call)) Log.i(TAG, "in a call with Muse") }
        }

        override fun onCallAudio(call: Long, pcm: ByteArray) {
            // Straight to the player, off the main thread: Muse's voice must not wait on the screen.
            if (calling && (callId == 0L || callId == call)) callPlayer?.write(pcm)
        }

        override fun onCallFlush(call: Long) {
            if (calling && (callId == 0L || callId == call)) callPlayer?.flush()
        }

        override fun onCallHeard(call: Long, text: String) {
            main.post { if (ourCall(call)) _state.update { it.copy(heard = text, answer = "") } }
        }

        override fun onCallSaid(call: Long, text: String) {
            main.post { if (ourCall(call)) _state.update { it.copy(answer = text, answerIsProblem = false) } }
        }

        override fun onCallHold(call: Long, held: Boolean) {
            if (calling && (callId == 0L || callId == call)) callHeld = held
        }

        override fun onCallStop(call: Long) {
            // Muse said goodbye: let it finish, then hang up.
            val p = callPlayer
            thread { p?.drain(5200); app?.hangUp() }
        }

        override fun onCallOpus(call: Long, packet: ByteArray) {
            if (calling && callVideo && (callId == 0L || callId == call)) opusIn?.feed(packet)
        }

        override fun onCallVideo(call: Long, unit: ByteArray, width: Long, height: Long) {
            if (calling && callVideo && (callId == 0L || callId == call)) picture?.feed(unit)
        }

        override fun onCallEnded(call: Long, reason: String, live: Boolean) {
            main.post {
                if (!ourCall(call)) return@post
                Log.i(TAG, "the call ended: $reason")
                val wasVideo = callVideo
                endCallHere()
                _state.update { it.copy(inCall = false, videoCall = false) }
                if (!live && wasVideo) {
                    // Muse would not take a video call: a voice-only call instead.
                    if (reason == Heymuse.CallDeclined) videoDeclinedAt = System.currentTimeMillis()
                    phase = Phase.IDLE
                    startCall()
                } else if (!live) {
                    if (reason == Heymuse.CallDeclined) callDeclinedAt = System.currentTimeMillis()
                    // Ask the old way instead.
                    phase = Phase.IDLE
                    listenForNote()
                } else {
                    toIdle()
                }
            }
        }

        override fun onTurnFailed(turn: Long, reason: String) {
            main.post { if (ours(turn)) problem(reason) }
        }

        override fun onTurnEnded(turn: Long) {
            main.post { if (ours(turn)) toIdle() }
        }

        override fun onDeviceChanged() = refreshDevice()

        override fun setVolume(level: Long): Long =
            Sound.setVolume(context, level.toInt(), Heymuse.VolumeSteps.toInt()).toLong()

        override fun volume(): Long = Sound.volume(context, Heymuse.VolumeSteps.toInt()).toLong()

        override fun ring(kind: String, label: String) {
            val what = (if (kind == "alarm") "Alarm" else "Timer") + if (label.isNotEmpty()) ": $label" else ""
            main.post {
                ringer?.start()
                _state.update { it.copy(ringing = what) }
                bringToFront()
            }
        }

        override fun stopRinging(): Boolean {
            val was = ringer?.stop() ?: false
            main.post { _state.update { it.copy(ringing = null) } }
            return was
        }

        override fun stopSpeaking() {
            voice?.stop()
        }

        override fun log(level: String, line: String) {
            when (level) {
                "error" -> Log.e("HeyMuseCore", line)
                "warn" -> Log.w("HeyMuseCore", line)
                "debug" -> Log.d("HeyMuseCore", line)
                else -> Log.i("HeyMuseCore", line)
            }
        }
    }
}
