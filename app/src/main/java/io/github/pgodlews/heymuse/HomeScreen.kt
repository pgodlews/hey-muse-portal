package io.github.pgodlews.heymuse

import androidx.compose.animation.core.LinearEasing
import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.IconButtonDefaults
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.TextUnit
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import kotlinx.coroutines.delay
import java.text.DateFormat
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import kotlin.math.PI
import kotlin.math.cos
import kotlin.math.sin

/**
 * Sizes for this screen. Everything is drawn for a Portal+ (720 dp on its short side) and scaled
 * by the screen's short side, so a Portal Go, Portal, Mini or TV gets the same proportions.
 * [wide] screens (3:2, 16:10, 16:9 landscape) put Muse beside the words; narrow or portrait ones
 * stack them.
 */
class Dims(val k: Float, val wide: Boolean) {
    fun d(v: Int): Dp = (v * k).dp
    fun t(v: Int): TextUnit = (v * k).sp
}

val LocalDims = staticCompositionLocalOf { Dims(1f, true) }

/** Works out [Dims] from the space a screen has. */
fun dimsFor(width: Dp, height: Dp): Dims {
    val short = minOf(width, height).value
    return Dims((short / 720f).coerceIn(0.72f, 1.3f), width.value / height.value >= 1.4f)
}

@Composable
fun HomeScreen(state: UiState, avatar: Avatar?, myMuse: MyMuseAssets?, captions: Boolean, wakeWord: Boolean, onSettings: () -> Unit) {
    val talking = state.inCall || state.phase == Phase.SPEAKING ||
        (state.phase == Phase.THINKING && state.heard.isNotEmpty())
    val background = if (state.inCall) listOf(Color(0xFF1E2E3A), Night) else listOf(Color(0xFF221B42), Night)
    BoxWithConstraints(
        Modifier
            .fillMaxSize()
            .background(Brush.radialGradient(background, radius = 1600f))
            .clickable(interactionSource = remember { MutableInteractionSource() }, indication = null) { Assistant.tap() }
    ) {
        val dims = dimsFor(maxWidth, maxHeight)
        val areaH = maxHeight
        val areaW = maxWidth
        CompositionLocalProvider(LocalDims provides dims) {
            val u = dims
            Column(Modifier.fillMaxSize().padding(horizontal = u.d(48), vertical = u.d(32))) {
                TopBar(state, onSettings)
                Box(Modifier.fillMaxWidth().weight(1f), contentAlignment = Alignment.Center) {
                    if (talking && dims.wide) {
                        // Muse on the left, the conversation on the right.
                        val face = minOf(areaH * 0.58f, areaW * 0.36f)
                        Row(Modifier.fillMaxSize(), verticalAlignment = Alignment.CenterVertically,
                            horizontalArrangement = Arrangement.spacedBy(u.d(40))) {
                            Box(Modifier.weight(1f).fillMaxHeight(), contentAlignment = Alignment.Center) {
                                Face(state, avatar, myMuse, face)
                            }
                            Box(Modifier.weight(1f).fillMaxHeight(), contentAlignment = Alignment.CenterStart) {
                                Words(state, captions, center = false)
                            }
                        }
                    } else if (talking) {
                        val face = minOf(areaH * 0.36f, areaW * 0.62f)
                        Column(Modifier.fillMaxSize(), horizontalAlignment = Alignment.CenterHorizontally) {
                            Spacer(Modifier.height(u.d(12)))
                            Face(state, avatar, myMuse, face)
                            Box(Modifier.fillMaxWidth().weight(1f).padding(top = u.d(16)), contentAlignment = Alignment.Center) {
                                Words(state, captions, center = true)
                            }
                        }
                    } else {
                        // Waiting: Muse in the middle, its name and one line on what to do.
                        val face = minOf(areaH * 0.46f, areaW * 0.62f)
                        Column(horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(u.d(20))) {
                            Face(state, avatar, myMuse, face)
                            Column(horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(u.d(6))) {
                                Text(museName(state), color = Ink, fontSize = u.t(34), fontWeight = FontWeight.SemiBold)
                                Text(idleHint(state, wakeWord), color = InkSoft, fontSize = u.t(22), textAlign = TextAlign.Center)
                            }
                        }
                    }
                }
                BottomBar(state)
            }
            state.ringing?.let { Ringing(it) }
        }
    }
}

private fun museName(state: UiState) = state.status.agentName.ifEmpty { "Muse" }

private fun idleHint(state: UiState, wakeWord: Boolean): String = when (state.phase) {
    Phase.LISTENING -> "Listening… tap when you're done"
    Phase.THINKING -> "Thinking…"
    else -> if (wakeWord && state.wakeReady) "Say “Hey Muse”, or tap to talk" else "Tap to talk"
}

@Composable
private fun Face(state: UiState, avatar: Avatar?, myMuse: MyMuseAssets?, size: Dp) {
    when {
        myMuse != null -> MyMuse(myMuse, state.phase, state.level, state.inCall, Modifier.size(size),
            videoCall = state.videoCall, videoWidth = state.videoWidth, videoHeight = state.videoHeight)
        avatar != null -> {
            val name = when (state.phase) {
                Phase.LISTENING -> Avatar.LISTENING
                Phase.THINKING -> Avatar.THINKING
                Phase.SPEAKING -> Avatar.TALKING
                Phase.IDLE -> Avatar.IDLE
            }
            avatar.Show(name, Modifier.size(size))
        }
        else -> Orb(state.phase, state.level, Modifier.size(size))
    }
}

/** What was heard, small, and Muse's words, large. Their box never changes the layout around it. */
@Composable
private fun Words(state: UiState, captions: Boolean, center: Boolean) {
    val u = LocalDims.current
    val align = if (center) TextAlign.Center else TextAlign.Start
    Column(Modifier.fillMaxWidth(), horizontalAlignment = if (center) Alignment.CenterHorizontally else Alignment.Start,
        verticalArrangement = Arrangement.spacedBy(u.d(16))) {
        if (captions && state.heard.isNotEmpty()) {
            Text("You: “${state.heard}”", color = InkSoft, fontSize = u.t(20), lineHeight = u.t(28), textAlign = align,
                maxLines = 2, overflow = TextOverflow.Ellipsis)
        }
        val answer = state.answer
        when {
            answer.isNotEmpty() && (captions || state.answerIsProblem) -> {
                val long = answer.length > 200
                Text(answer, color = if (state.answerIsProblem) Warning else Ink,
                    fontSize = if (long) u.t(24) else u.t(30), lineHeight = if (long) u.t(33) else u.t(40),
                    fontWeight = FontWeight.Medium, textAlign = align, maxLines = if (u.wide) 8 else 6, overflow = TextOverflow.Ellipsis)
            }
            state.inCall && state.videoCall -> Text(
                if (state.phase == Phase.SPEAKING) "${museName(state)} is talking" else "${museName(state)} is listening",
                color = InkSoft, fontSize = u.t(24), textAlign = align)
            state.phase == Phase.THINKING -> Text("Thinking…", color = Accent, fontSize = u.t(26), textAlign = align)
            state.inCall -> Text("Listening…", color = Accent, fontSize = u.t(26), textAlign = align)
        }
    }
}

@Composable
private fun TopBar(state: UiState, onSettings: () -> Unit) {
    val u = LocalDims.current
    Row(Modifier.fillMaxWidth().height(u.d(80)), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(u.d(14))) {
        if (state.inCall) CallPill(state) else Clock()
        Spacer(Modifier.weight(1f))
        // A problem with the microphone shows here, in a pill of its own: nothing else moves.
        micWarningShort(state)?.let { Pill(it, Warning, Warning.copy(alpha = 0.14f)) }
        if (!state.inCall) StatusPill(state)
        IconButton(onClick = onSettings, modifier = Modifier.size(u.d(56)),
            colors = IconButtonDefaults.iconButtonColors(containerColor = Glass, contentColor = Ink)) {
            Icon(Icons.Filled.Settings, contentDescription = "Settings", modifier = Modifier.size(u.d(28)))
        }
    }
}

@Composable
private fun Clock() {
    val u = LocalDims.current
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(Unit) {
        while (true) {
            now = System.currentTimeMillis()
            delay(10_000)
        }
    }
    Column {
        Text(DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(now)), color = Ink, fontSize = u.t(44),
            fontWeight = FontWeight.Medium, lineHeight = u.t(46))
        Text(SimpleDateFormat("EEEE d MMMM", Locale.getDefault()).format(Date(now)), color = InkSoft, fontSize = u.t(18))
    }
}

@Composable
private fun CallPill(state: UiState) {
    var started by remember { mutableLongStateOf(System.currentTimeMillis()) }
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(Unit) {
        started = System.currentTimeMillis()
        while (true) {
            now = System.currentTimeMillis()
            delay(1000)
        }
    }
    val what = if (state.videoCall) "Video call with ${museName(state)}" else "In a call with ${museName(state)}"
    Pill("$what · ${countdown(now - started + 1000)}", Good, Good.copy(alpha = 0.14f), dot = Good)
}

@Composable
fun StatusPill(state: UiState) {
    val (text, color) = statusLine(state)
    Pill(text, Ink, Glass, dot = color)
}

/** The status chip some older screens still use. */
@Composable
fun StatusChip(state: UiState) = StatusPill(state)

@Composable
fun Pill(text: String, ink: Color, fill: Color, dot: Color? = null) {
    val u = LocalDims.current
    Surface(shape = RoundedCornerShape(50), color = fill) {
        Row(Modifier.height(u.d(44)).padding(horizontal = u.d(18)), verticalAlignment = Alignment.CenterVertically) {
            if (dot != null) {
                Box(Modifier.size(u.d(10)).clip(CircleShape).background(dot))
                Spacer(Modifier.size(u.d(10)))
            }
            Text(text, color = ink, fontSize = u.t(18), maxLines = 1, overflow = TextOverflow.Ellipsis)
        }
    }
}

fun statusLine(state: UiState): Pair<String, Color> {
    val s = state.status
    return when {
        s.pairing -> "Pairing" to Accent
        s.online -> s.agentName.ifEmpty { "Muse" } to Good
        s.phase == heymuse.Heymuse.PhaseConnecting -> "Connecting" to Warning
        s.paired -> "Offline" to Warning
        else -> "Not paired" to InkMuted
    }
}

@Composable
private fun BottomBar(state: UiState) {
    val u = LocalDims.current
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(state.timers.isNotEmpty()) {
        while (state.timers.isNotEmpty()) {
            now = System.currentTimeMillis()
            delay(250)
        }
    }
    // Always the same height, whatever it holds.
    Row(Modifier.fillMaxWidth().height(u.d(76)), horizontalArrangement = Arrangement.spacedBy(u.d(14), Alignment.CenterHorizontally),
        verticalAlignment = Alignment.CenterVertically) {
        when {
            state.inCall -> {
                if (!state.videoCall && state.phase == Phase.SPEAKING) {
                    BigButton("Stop talking", Glass, Ink) { Assistant.interrupt() }
                }
                BigButton("End call", Hangup, Color(0xFF2A0E06)) { Assistant.hangUp() }
            }
            state.phase == Phase.SPEAKING -> BigButton("Stop", Glass, Ink) { Assistant.tap() }
            else -> {
                state.timers.take(3).forEach { t -> Pill("${t.label.ifEmpty { "Timer" }} · ${countdown(t.endMs - now)}", Ink, Glass) }
                state.alarms.filter { it.on }.minByOrNull { it.nextMs }?.let { a ->
                    Pill("Alarm ${DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(a.nextMs))}${if (a.label.isNotEmpty()) " · ${a.label}" else ""}", Ink, Glass)
                }
            }
        }
    }
}

@Composable
private fun BigButton(text: String, fill: Color, ink: Color, onClick: () -> Unit) {
    val u = LocalDims.current
    Button(onClick = onClick, colors = ButtonDefaults.buttonColors(containerColor = fill, contentColor = ink),
        shape = RoundedCornerShape(50), modifier = Modifier.height(u.d(68)).widthIn(min = u.d(180))) {
        Text(text, fontSize = u.t(22), fontWeight = FontWeight.SemiBold)
    }
}

/** The microphone's problem in a few words, for the pill at the top. */
fun micWarningShort(state: UiState): String? = when (state.mic) {
    Mic.State.NO_PERMISSION -> "Microphone not allowed"
    Mic.State.SILENCED -> "Mic muted? Check the privacy switch"
    Mic.State.FAILED -> "Microphone unavailable"
    else -> null
}

fun micWarning(state: UiState): String? = when (state.mic) {
    Mic.State.NO_PERMISSION -> "Hey Muse needs the microphone. Allow it when asked, or install with adb install -g."
    Mic.State.SILENCED -> "The microphone hears nothing. Check the privacy switch, or turn off “Hey Portal” in the Portal's settings."
    Mic.State.FAILED -> "The microphone can't be opened right now. Hey Muse keeps trying."
    else -> null
}

fun countdown(ms: Long): String {
    val s = (ms.coerceAtLeast(0) + 999) / 1000
    val h = s / 3600
    val m = s % 3600 / 60
    val sec = s % 60
    return if (h > 0) "%d:%02d:%02d".format(h, m, sec) else "%d:%02d".format(m, sec)
}

/** Muse's stand-in face when there is no character: a ring of light that breathes, listens and talks. */
@Composable
private fun Orb(phase: Phase, level: Float, modifier: Modifier) {
    val loop = rememberInfiniteTransition(label = "orb")
    val breath by loop.animateFloat(0f, 1f, infiniteRepeatable(tween(3200, easing = LinearEasing), RepeatMode.Reverse), label = "breath")
    val spin by loop.animateFloat(0f, 1f, infiniteRepeatable(tween(1400, easing = LinearEasing)), label = "spin")
    val loud by animateFloatAsState(if (phase == Phase.LISTENING || phase == Phase.SPEAKING) level else 0f, tween(90), label = "loud")
    val glow by animateFloatAsState(if (phase == Phase.IDLE) 0.35f else 1f, tween(400), label = "glow")
    Canvas(modifier) {
        val r = size.minDimension / 2
        val c = center
        val color = if (phase == Phase.SPEAKING) AccentWarm else Accent
        val core = r * (0.46f + 0.04f * breath + 0.22f * loud)
        drawCircle(Brush.radialGradient(listOf(color.copy(alpha = 0.55f * glow), Color.Transparent), c, r), r, c)
        drawCircle(Brush.radialGradient(listOf(Color.White.copy(alpha = 0.9f * glow), color.copy(alpha = glow), color.copy(alpha = 0.15f)), c, core), core, c)
        if (phase == Phase.THINKING) {
            val ring = r * 0.72f
            for (i in 0 until 3) {
                val a = 2 * PI * (spin + i / 3.0)
                drawCircle(Accent, r * 0.05f, Offset(c.x + ring * cos(a).toFloat(), c.y + ring * sin(a).toFloat()))
            }
        }
        if (phase == Phase.LISTENING) {
            drawCircle(color.copy(alpha = 0.6f), core + r * 0.1f + r * 0.2f * loud, c, style = Stroke(width = r * 0.02f))
        }
    }
}

@Composable
private fun Ringing(what: String) {
    val u = LocalDims.current
    Box(
        Modifier.fillMaxSize().background(Night.copy(alpha = 0.94f))
            .clickable(interactionSource = remember { MutableInteractionSource() }, indication = null) { Assistant.stopRinging() },
        contentAlignment = Alignment.Center,
    ) {
        Column(horizontalAlignment = Alignment.CenterHorizontally) {
            Text(what, color = Ink, fontSize = u.t(48), fontWeight = FontWeight.SemiBold, textAlign = TextAlign.Center)
            Spacer(Modifier.height(u.d(28)))
            BigButton("Stop", Hangup, Color(0xFF2A0E06)) { Assistant.stopRinging() }
            Spacer(Modifier.height(u.d(16)))
            Text("or say “Hey Muse”", color = InkSoft, fontSize = u.t(20))
        }
    }
}
