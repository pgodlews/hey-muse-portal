package io.github.pgodlews.heymuse

import android.media.MediaCodec
import android.media.MediaFormat
import android.util.Log
import android.view.Surface
import java.nio.ByteBuffer
import java.nio.ByteOrder

/**
 * The codecs of a video call with Muse, on the Portal's own MediaCodecs: Muse's voice comes as Opus
 * and is played at 48 kHz, the caller's voice goes as Opus, and Muse's picture comes as H.264 and is
 * drawn on a surface.
 */
object Opus {
    const val RATE = 48000
    /** 20 ms of 48 kHz mono, 16-bit. */
    const val FRAME_BYTES = RATE / 50 * 2
    private const val PRE_SKIP = 312

    /** The Opus identification header, which Android's decoder needs as csd-0. */
    private fun head(): ByteBuffer = ByteBuffer.allocate(19).order(ByteOrder.LITTLE_ENDIAN).apply {
        put("OpusHead".toByteArray())
        put(1)                 // version
        put(1)                 // channels
        putShort(PRE_SKIP.toShort())
        putInt(RATE)
        putShort(0)            // gain
        put(0)                 // mapping family
        flip()
    }

    private fun nanos(n: Long): ByteBuffer = ByteBuffer.allocate(8).order(ByteOrder.LITTLE_ENDIAN).apply { putLong(n); flip() }

    /** Decodes Muse's Opus packets into 48 kHz PCM for [out]. Feed it from one thread. */
    class Decoder(private val out: (ByteArray) -> Unit) {
        private val codec = MediaCodec.createDecoderByType(MediaFormat.MIMETYPE_AUDIO_OPUS).apply {
            val f = MediaFormat.createAudioFormat(MediaFormat.MIMETYPE_AUDIO_OPUS, RATE, 1)
            f.setByteBuffer("csd-0", head())
            f.setByteBuffer("csd-1", nanos(PRE_SKIP * 1_000_000_000L / RATE))
            f.setByteBuffer("csd-2", nanos(80_000_000L))
            configure(f, null, null, 0)
            start()
        }
        private val info = MediaCodec.BufferInfo()
        private var pts = 0L
        @Volatile private var closed = false

        fun feed(packet: ByteArray) {
            if (closed) return
            try {
                val i = codec.dequeueInputBuffer(20_000)
                if (i >= 0) {
                    codec.getInputBuffer(i)!!.apply { clear(); put(packet) }
                    codec.queueInputBuffer(i, 0, packet.size, pts, 0)
                    pts += 20_000
                }
                drain()
            } catch (e: Exception) {
                if (!closed) Log.w(TAG, "an Opus packet did not decode", e)
            }
        }

        private fun drain() {
            while (true) {
                val o = codec.dequeueOutputBuffer(info, 0)
                if (o < 0) return
                if (info.size > 0) {
                    val b = codec.getOutputBuffer(o)!!
                    val pcm = ByteArray(info.size)
                    b.position(info.offset)
                    b.get(pcm)
                    out(pcm)
                }
                codec.releaseOutputBuffer(o, false)
            }
        }

        fun close() {
            closed = true
            runCatching { codec.stop() }
            runCatching { codec.release() }
        }
    }

    /** Encodes the caller's 48 kHz PCM, in 20 ms frames, into Opus packets for [out]. Feed it from one thread. */
    class Encoder(private val out: (ByteArray) -> Unit) {
        private val codec = MediaCodec.createEncoderByType(MediaFormat.MIMETYPE_AUDIO_OPUS).apply {
            val f = MediaFormat.createAudioFormat(MediaFormat.MIMETYPE_AUDIO_OPUS, RATE, 1)
            f.setInteger(MediaFormat.KEY_BIT_RATE, 32_000)
            configure(f, null, null, MediaCodec.CONFIGURE_FLAG_ENCODE)
            start()
        }
        private val info = MediaCodec.BufferInfo()
        private var pts = 0L
        private val pending = java.io.ByteArrayOutputStream()
        @Volatile private var closed = false

        /** Takes 48 kHz PCM; whole 20 ms frames go to the encoder. */
        fun feed(pcm: ByteArray) {
            if (closed) return
            pending.write(pcm)
            val all = pending.toByteArray()
            var off = 0
            try {
                while (all.size - off >= FRAME_BYTES) {
                    val i = codec.dequeueInputBuffer(20_000)
                    if (i < 0) break
                    codec.getInputBuffer(i)!!.apply { clear(); put(all, off, FRAME_BYTES) }
                    codec.queueInputBuffer(i, 0, FRAME_BYTES, pts, 0)
                    pts += 20_000
                    off += FRAME_BYTES
                    drain()
                }
            } catch (e: Exception) {
                if (!closed) Log.w(TAG, "the microphone did not encode", e)
            }
            pending.reset()
            pending.write(all, off, all.size - off)
        }

        private fun drain() {
            while (true) {
                val o = codec.dequeueOutputBuffer(info, 0)
                if (o < 0) return
                // The codec's own header is not a packet to send.
                if (info.size > 0 && info.flags and MediaCodec.BUFFER_FLAG_CODEC_CONFIG == 0) {
                    val b = codec.getOutputBuffer(o)!!
                    val packet = ByteArray(info.size)
                    b.position(info.offset)
                    b.get(packet)
                    out(packet)
                }
                codec.releaseOutputBuffer(o, false)
            }
        }

        fun close() {
            closed = true
            runCatching { codec.stop() }
            runCatching { codec.release() }
        }
    }

    private const val TAG = "HeyMuseOpus"
}

/** Brings 16 kHz microphone audio up to 48 kHz: three samples out for each one in, by linear steps. */
class Upsample16to48 {
    private var last = 0
    fun feed(pcm: ByteArray, n: Int): ByteArray {
        val out = ByteArray(n / 2 * 6)
        var o = 0
        var i = 0
        while (i + 1 < n) {
            val s = ((pcm[i + 1].toInt() shl 8) or (pcm[i].toInt() and 0xff)).toShort().toInt()
            for (k in 1..3) {
                val v = last + (s - last) * k / 3
                out[o++] = v.toByte(); out[o++] = (v shr 8).toByte()
            }
            last = s
            i += 2
        }
        return out
    }
}

/**
 * Draws Muse's H.264 picture on [surface]. Access units come from the call's goroutine, one at a
 * time. A decoder that has not yet seen a whole picture asks for one with [askKeyframe].
 */
class VideoDecoder(private val surface: Surface, private val askKeyframe: () -> Unit, private val size: (Int, Int) -> Unit) {
    private var codec: MediaCodec? = null
    private val info = MediaCodec.BufferInfo()
    private var pts = 0L
    private var started = false
    @Volatile private var closed = false

    fun feed(unit: ByteArray) {
        if (closed) return
        try {
            if (!started) {
                // Start on a picture that stands alone, with its parameter sets.
                if (!hasKeyframe(unit)) {
                    askKeyframe()
                    return
                }
                codec = MediaCodec.createDecoderByType(MediaFormat.MIMETYPE_VIDEO_AVC).apply {
                    val f = MediaFormat.createVideoFormat(MediaFormat.MIMETYPE_VIDEO_AVC, 720, 1280)
                    configure(f, surface, null, 0)
                    start()
                }
                started = true
            }
            val c = codec ?: return
            val i = c.dequeueInputBuffer(10_000)
            if (i >= 0) {
                c.getInputBuffer(i)!!.apply { clear(); put(unit) }
                c.queueInputBuffer(i, 0, unit.size, pts, if (hasKeyframe(unit)) MediaCodec.BUFFER_FLAG_KEY_FRAME else 0)
                pts += 33_333
            }
            while (true) {
                val o = c.dequeueOutputBuffer(info, 0)
                if (o == MediaCodec.INFO_OUTPUT_FORMAT_CHANGED) {
                    val f = c.outputFormat
                    val w = f.getInteger(MediaFormat.KEY_WIDTH)
                    val h = f.getInteger(MediaFormat.KEY_HEIGHT)
                    size(w, h)
                    continue
                }
                if (o < 0) break
                c.releaseOutputBuffer(o, true)
            }
        } catch (e: Exception) {
            if (!closed) {
                Log.w("HeyMuseVideo", "a picture did not decode; asking for a whole one", e)
                reset()
                askKeyframe()
            }
        }
    }

    private fun reset() {
        codec?.let { runCatching { it.stop() }; runCatching { it.release() } }
        codec = null
        started = false
    }

    fun close() {
        closed = true
        reset()
    }

    companion object {
        /** Whether an Annex-B access unit holds an IDR picture. */
        fun hasKeyframe(unit: ByteArray): Boolean {
            var i = 0
            while (i + 3 < unit.size) {
                if (unit[i].toInt() == 0 && unit[i + 1].toInt() == 0 &&
                    (unit[i + 2].toInt() == 1 || (unit[i + 2].toInt() == 0 && unit[i + 3].toInt() == 1))
                ) {
                    val start = if (unit[i + 2].toInt() == 1) i + 3 else i + 4
                    if (start < unit.size && unit[start].toInt() and 0x1f == 5) return true
                    i = start
                } else {
                    i++
                }
            }
            return false
        }
    }
}
