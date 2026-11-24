package io.github.pgodlews.heymuse

import android.media.AudioTrack
import android.util.Log
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicLong
import kotlin.concurrent.thread

/**
 * Plays Muse's voice during a live call: 24 kHz PCM as it arrives, in order, on one track that lives
 * as long as the call. [flush] drops what has not been heard yet (Muse was cut off).
 *
 * It also says whether Muse can be heard in the room ([audible]), so the microphone can leave out
 * the Portal's own voice: half duplex, as Meta's SDK does by default.
 */
class CallPlayer(private val rate: Int, private val level: (Float) -> Unit) {
    private val queue = LinkedBlockingQueue<ByteArray>()
    @Volatile private var running = true
    @Volatile private var track: AudioTrack? = null
    /** Bumped by flush, so a chunk already taken off the queue is not written after it. */
    private val epoch = AtomicLong()
    /** Frames written to the track, and when audible audio is last due to finish. */
    private var written = 0L
    @Volatile private var audibleUntil = 0L
    @Volatile private var queuedBytes = 0L
    private var lastLevelAt = 0L

    private val worker = thread(name = "muse-call-player") { run() }

    /** Queues Muse's voice. Silence with nothing playing is left out: Muse sends keepalives. */
    fun write(pcm: ByteArray) {
        if (!running || pcm.isEmpty()) return
        if (queuedBytes == 0L && !playing() && pcm.all { it == 0.toByte() }) return
        queuedBytes += pcm.size
        queue.offer(pcm)
    }

    /** Drops everything not heard yet. */
    fun flush() {
        epoch.incrementAndGet()
        queue.clear()
        queuedBytes = 0
        track?.let { t -> runCatching { t.pause(); t.flush(); t.play() } }
        synchronized(this) { written = 0 }
        audibleUntil = 0
        level(0f)
    }

    /** Whether Muse's voice is in the room now, or was a moment ago. */
    fun audible(): Boolean = System.currentTimeMillis() < audibleUntil + TAIL_MS

    /** Whether anything is still to be heard. */
    fun playing(): Boolean = queuedBytes > 0 || System.currentTimeMillis() < audibleUntil

    /** Waits up to [ms] for what is queued to be heard. */
    fun drain(ms: Long) {
        val deadline = System.currentTimeMillis() + ms
        while (playing() && System.currentTimeMillis() < deadline) Thread.sleep(40)
    }

    fun stop() {
        running = false
        queue.clear()
        queuedBytes = 0
        worker.interrupt()
    }

    private fun run() {
        val t = Sound.stream(rate)
        track = t
        try {
            t.play()
            while (running) {
                val pcm = try {
                    queue.poll(200, TimeUnit.MILLISECONDS)
                } catch (e: InterruptedException) {
                    break
                } ?: continue
                val mine = epoch.get()
                queuedBytes = maxOf(0, queuedBytes - pcm.size)
                val loud = Voice.loudness(pcm, pcm.size)
                if (loud > 0.05f) {
                    // Due to finish when everything written so far has played, plus this chunk.
                    val ahead = synchronized(this) { written - t.playbackHeadPosition.toLong() }.coerceAtLeast(0)
                    audibleUntil = System.currentTimeMillis() + (ahead + pcm.size / 2) * 1000 / rate
                }
                val now = System.currentTimeMillis()
                if (now - lastLevelAt > 40) {
                    lastLevelAt = now
                    level(loud)
                }
                var off = 0
                while (off < pcm.size && running && epoch.get() == mine) {
                    val w = t.write(pcm, off, pcm.size - off)
                    if (w < 0) break
                    off += w
                }
                synchronized(this) { written += off / 2 }
            }
        } catch (e: Exception) {
            Log.w(TAG, "the call's playback failed", e)
        } finally {
            level(0f)
            runCatching { t.stop() }
            t.release()
            track = null
        }
    }

    companion object {
        private const val TAG = "HeyMuseCall"
        /** How long after Muse's voice the microphone stays shut: the room's echo. */
        const val TAIL_MS = 300L
    }
}
