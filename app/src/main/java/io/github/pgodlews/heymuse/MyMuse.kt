package io.github.pgodlews.heymuse

import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.graphics.SurfaceTexture
import android.media.MediaPlayer
import android.util.Log
import android.view.Surface
import android.view.TextureView
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.scale
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import kotlinx.coroutines.delay
import org.json.JSONObject
import java.io.File

/**
 * The owner's own Muse as the Muse app shows it: the still and the phone app's animation clips,
 * fetched from their Muse by Assistant.syncAvatar into files/avatar. They are the owner's and are
 * never part of this app.
 */
class MyMuseAssets(val dir: File, val name: String, val still: File?, val clips: Map<String, File>) {
    companion object {
        fun load(dir: File): MyMuseAssets? = runCatching {
            val m = JSONObject(File(dir, "manifest.json").readText())
            val still = m.optString("still").takeIf { it.isNotEmpty() }?.let { File(dir, it) }?.takeIf { it.length() > 0 }
            val clips = mutableMapOf<String, File>()
            m.optJSONObject("clips")?.let { c -> c.keys().forEach { k -> File(dir, c.getString(k)).takeIf { it.length() > 0 }?.let { clips[k] = it } } }
            if (still == null && clips.isEmpty()) null else MyMuseAssets(dir, m.optString("name"), still, clips)
        }.getOrNull()
    }
}

/** Which clip shows for a moment of the conversation. */
private fun clipFor(phase: Phase, thinkingLong: Boolean, celebrate: Boolean, clips: Map<String, File>): File? {
    val want = when {
        celebrate -> "milestone_level_up"
        phase == Phase.THINKING && thinkingLong -> "waiting_for_subagents"
        phase == Phase.THINKING -> "working"
        else -> "idle"
    }
    return clips[want] ?: clips["working"].takeIf { phase == Phase.THINKING } ?: clips["idle"]
}

@Composable
fun MyMuse(assets: MyMuseAssets, phase: Phase, level: Float, inCall: Boolean, modifier: Modifier,
           videoCall: Boolean = false, videoWidth: Int = 0, videoHeight: Int = 0) {
    // A long think gets the waiting clip; a call that has just connected gets the celebration, once.
    var thinkingLong by remember { mutableStateOf(false) }
    LaunchedEffect(phase) {
        thinkingLong = false
        if (phase == Phase.THINKING) {
            delay(6000)
            thinkingLong = true
        }
    }
    var celebrate by remember { mutableStateOf(false) }
    LaunchedEffect(inCall) {
        if (inCall && assets.clips.containsKey("milestone_level_up")) {
            celebrate = true
            delay(2500)
            celebrate = false
        }
    }
    val clip = clipFor(phase, thinkingLong, celebrate, assets.clips)

    // Listening and speaking show as a ring and a small pulse with the voice: the clips have no mouth.
    val pulse by animateFloatAsState(
        targetValue = if (phase == Phase.SPEAKING) 1f + 0.05f * level else 1f,
        animationSpec = tween(90), label = "pulse",
    )
    val ringColor = when (phase) {
        Phase.LISTENING -> Accent
        Phase.SPEAKING -> Color(0xFF7DE2A6)
        Phase.THINKING -> Warning
        Phase.IDLE -> Color.White.copy(alpha = 0.18f)
    }
    val ringWidth by animateFloatAsState(
        targetValue = when (phase) {
            Phase.LISTENING -> 4f + 10f * level
            Phase.SPEAKING -> 4f + 8f * level
            Phase.THINKING -> 5f
            Phase.IDLE -> 2f
        },
        animationSpec = tween(120), label = "ring",
    )

    Box(modifier.scale(pulse), contentAlignment = Alignment.Center) {
        Box(
            Modifier
                .fillMaxSize()
                .clip(CircleShape)
                .background(Color.White)
                .border(ringWidth.dp, ringColor, CircleShape),
            contentAlignment = Alignment.Center,
        ) {
            if (clip != null) {
                ClipPlayer(clip, assets.still, Modifier.fillMaxSize().clip(CircleShape))
            }
            if (videoCall) {
                // Muse talking, live, over the clip once its first picture is in.
                CallVideo(videoWidth, videoHeight, Modifier.fillMaxSize().clip(CircleShape))
            }
            if (clip == null && assets.still != null) {
                val bmp = remember(assets.still) { BitmapFactory.decodeFile(assets.still.path) }
                if (bmp != null) Image(bmp.asImageBitmap(), contentDescription = assets.name, contentScale = ContentScale.Crop,
                    modifier = Modifier.fillMaxSize().clip(CircleShape))
            }
        }
    }
}

/**
 * Plays [clip] on a loop. A change of clip holds the last frame on screen until the new one has
 * drawn, so switching never flashes.
 */
@Composable
private fun ClipPlayer(clip: File, still: File?, modifier: Modifier) {
    var held by remember { mutableStateOf<Bitmap?>(null) }
    val holder = remember { PlayerHolder() }
    DisposableEffect(Unit) { onDispose { holder.release() } }
    Box(modifier) {
        AndroidView(
            factory = { ctx ->
                TextureView(ctx).also { tv ->
                    holder.view = tv
                    tv.surfaceTextureListener = object : TextureView.SurfaceTextureListener {
                        override fun onSurfaceTextureAvailable(st: SurfaceTexture, w: Int, h: Int) {
                            holder.surface = Surface(st)
                            holder.play(holder.want ?: return) { held = null }
                        }
                        override fun onSurfaceTextureSizeChanged(st: SurfaceTexture, w: Int, h: Int) {}
                        override fun onSurfaceTextureDestroyed(st: SurfaceTexture): Boolean {
                            holder.release()
                            return true
                        }
                        override fun onSurfaceTextureUpdated(st: SurfaceTexture) {}
                    }
                }
            },
            modifier = Modifier.fillMaxSize(),
        )
        held?.let { Image(it.asImageBitmap(), contentDescription = null, contentScale = ContentScale.Crop, modifier = Modifier.fillMaxSize()) }
    }
    LaunchedEffect(clip) {
        if (holder.want == clip) return@LaunchedEffect
        if (holder.playing != null) held = holder.view?.bitmap
        holder.want = clip
        if (holder.surface != null) holder.play(clip) { held = null }
    }
}

private class PlayerHolder {
    var view: TextureView? = null
    var surface: Surface? = null
    var want: File? = null
    var playing: File? = null
    private var player: MediaPlayer? = null

    fun play(clip: File, drawn: () -> Unit) {
        val s = surface ?: return
        player?.release()
        player = null
        playing = null
        runCatching {
            player = MediaPlayer().apply {
                setSurface(s)
                setDataSource(clip.path)
                isLooping = true
                setVolume(0f, 0f)
                setOnInfoListener { _, what, _ ->
                    if (what == MediaPlayer.MEDIA_INFO_VIDEO_RENDERING_START) drawn()
                    false
                }
                setOnErrorListener { _, what, extra ->
                    Log.w("HeyMuseAvatar", "a clip did not play ($what/$extra)")
                    drawn()
                    true
                }
                setOnPreparedListener { it.start() }
                prepareAsync()
            }
            playing = clip
        }.onFailure {
            Log.w("HeyMuseAvatar", "a clip did not play", it)
            drawn()
        }
    }

    fun release() {
        player?.release()
        player = null
        playing = null
        surface?.release()
        surface = null
    }
}

/** Muse's live picture in a video call, cropped to fill, hidden until it has drawn. */
@Composable
private fun CallVideo(width: Int, height: Int, modifier: Modifier) {
    val shown = width > 0 && height > 0
    AndroidView(
        factory = { ctx ->
            TextureView(ctx).also { tv ->
                tv.surfaceTextureListener = object : TextureView.SurfaceTextureListener {
                    var surface: Surface? = null
                    override fun onSurfaceTextureAvailable(st: SurfaceTexture, w: Int, h: Int) {
                        surface = Surface(st)
                        Assistant.attachVideo(surface)
                    }
                    override fun onSurfaceTextureSizeChanged(st: SurfaceTexture, w: Int, h: Int) {}
                    override fun onSurfaceTextureDestroyed(st: SurfaceTexture): Boolean {
                        Assistant.attachVideo(null)
                        surface?.release()
                        return true
                    }
                    override fun onSurfaceTextureUpdated(st: SurfaceTexture) {}
                }
            }
        },
        update = { tv ->
            tv.alpha = if (shown) 1f else 0f
            if (shown && tv.width > 0 && tv.height > 0) {
                // Fill the view, keeping the picture's shape. Muse's picture is portrait with the face
                // near the top, so a tall picture is anchored a little below its top, not centred.
                val vw = tv.width.toFloat()
                val vh = tv.height.toFloat()
                val scale = maxOf(vw / width, vh / height)
                val m = android.graphics.Matrix()
                m.setScale(width * scale / vw, height * scale / vh, vw / 2, 0f)
                val spare = height * scale - vh
                if (spare > 0) m.postTranslate(0f, -spare * 0.12f)
                tv.setTransform(m)
            }
        },
        modifier = modifier,
    )
}
