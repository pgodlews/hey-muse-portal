package io.github.pgodlews.heymuse

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.filled.KeyboardArrowRight
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.IconButtonDefaults
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Switch
import androidx.compose.material3.SwitchDefaults
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.scale
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.unit.sp
import heymuse.Heymuse
import io.github.pgodlews.heymuse.server.SetupServer

private enum class Page { MAIN, ADVANCED, ABOUT }

/**
 * Settings, in plain words and big rows: the few things people change day to day on the first page,
 * and everything technical (pairing, the token, cloud voices, the Portal's own switches) on Advanced.
 */
@Composable
fun SettingsScreen(
    state: UiState,
    settings: Settings,
    avatarLoaded: Boolean,
    avatarPath: String,
    canOverlay: Boolean,
    onAllowOverlay: () -> Unit,
    onKeepScreenOn: () -> Unit,
    onClose: () -> Unit,
) {
    // Settings are plain preferences; this counter redraws the screen after one is written.
    var version by remember { mutableIntStateOf(0) }
    fun changed(mic: Boolean = false) {
        version++
        Assistant.settingsChanged(micChanged = mic)
    }
    var page by remember { mutableStateOf(Page.MAIN) }
    BackHandler(enabled = page != Page.MAIN) { page = Page.MAIN }

    val context = LocalContext.current
    var setupServer by remember { mutableStateOf<SetupServer?>(null) }
    fun phoneSetup() {
        val server = SetupServer(context, settings) { changed(false) }
        if (server.start()) setupServer = server
    }
    setupServer?.let { server ->
        QrCodeDialog(server = server, onDismiss = { server.stop(); setupServer = null })
    }

    BoxWithConstraints(Modifier.fillMaxSize().background(Night)) {
        val dims = dimsFor(maxWidth, maxHeight)
        val twoColumns = maxWidth.value >= 820f
        CompositionLocalProvider(LocalDims provides dims) {
            val u = dims
            Column(Modifier.fillMaxSize().padding(horizontal = u.d(40), vertical = u.d(24))) {
                Header(
                    title = when (page) { Page.MAIN -> "Settings"; Page.ADVANCED -> "Advanced"; Page.ABOUT -> "About" },
                    state = state,
                    onBack = { if (page == Page.MAIN) onClose() else page = Page.MAIN },
                )
                Spacer(Modifier.height(u.d(18)))
                Column(Modifier.fillMaxWidth().weight(1f).verticalScroll(rememberScrollState())) {
                    key(version) {
                        when (page) {
                            Page.MAIN -> MainPage(state, settings, twoColumns, ::changed, onKeepScreenOn,
                                openAdvanced = { page = Page.ADVANCED }, openAbout = { page = Page.ABOUT })
                            Page.ADVANCED -> AdvancedPage(state, settings, twoColumns, canOverlay, onAllowOverlay, onKeepScreenOn, ::changed, ::phoneSetup)
                            Page.ABOUT -> AboutPage(avatarLoaded, avatarPath)
                        }
                    }
                    Spacer(Modifier.height(u.d(24)))
                }
            }
        }
    }
}

@Composable
private fun Header(title: String, state: UiState, onBack: () -> Unit) {
    val u = LocalDims.current
    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(u.d(16))) {
        IconButton(onClick = onBack, modifier = Modifier.size(u.d(56)),
            colors = IconButtonDefaults.iconButtonColors(containerColor = Glass, contentColor = Ink)) {
            @Suppress("DEPRECATION")
            Icon(Icons.Filled.ArrowBack, contentDescription = "Back", modifier = Modifier.size(u.d(28)))
        }
        Text(title, color = Ink, fontSize = u.t(34), fontWeight = FontWeight.SemiBold)
        Spacer(Modifier.weight(1f))
        val s = state.status
        when {
            s.online -> Pill("Connected to ${s.agentName.ifEmpty { "Muse" }}", Good, Good.copy(alpha = 0.12f), dot = Good)
            else -> StatusPill(state)
        }
    }
}

// ---- Building blocks ----

/** Two columns on a wide screen, one on a narrow one. */
@Composable
private fun Columns(two: Boolean, left: @Composable ColumnScope.() -> Unit, right: @Composable ColumnScope.() -> Unit) {
    val u = LocalDims.current
    if (two) {
        Row(horizontalArrangement = Arrangement.spacedBy(u.d(20))) {
            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(u.d(10)), content = left)
            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(u.d(10)), content = right)
        }
    } else {
        Column(verticalArrangement = Arrangement.spacedBy(u.d(10))) {
            left()
            Spacer(Modifier.height(u.d(10)))
            right()
        }
    }
}

@Composable
private fun SectionTitle(text: String, top: Boolean = false) {
    val u = LocalDims.current
    Text(text.uppercase(), color = Accent, fontSize = u.t(17), fontWeight = FontWeight.SemiBold, letterSpacing = 0.6.sp,
        modifier = Modifier.padding(start = u.d(8), top = if (top) u.d(12) else u.d(0)))
}

/** A rounded card of rows, with a hairline between them. */
@Composable
private fun Card(rows: List<@Composable () -> Unit>) {
    val u = LocalDims.current
    Column(Modifier.fillMaxWidth().clip(RoundedCornerShape(u.d(24))).background(Glass)) {
        rows.forEachIndexed { i, row ->
            if (i > 0) HorizontalDivider(color = Color.White.copy(alpha = 0.07f))
            row()
        }
    }
}

@Composable
private fun Row(title: String, detail: String? = null, onClick: (() -> Unit)? = null, trailing: @Composable () -> Unit = {}) {
    val u = LocalDims.current
    Row(
        Modifier.fillMaxWidth().heightIn(min = u.d(76)).let { if (onClick != null) it.clickable(onClick = onClick) else it }
            .padding(horizontal = u.d(24), vertical = u.d(10)),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(u.d(16)),
    ) {
        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(u.d(3))) {
            Text(title, color = Ink, fontSize = u.t(22), fontWeight = FontWeight.Medium)
            if (detail != null) Text(detail, color = InkSoft, fontSize = u.t(17))
        }
        trailing()
    }
}

@Composable
private fun ToggleRow(title: String, detail: String? = null, checked: Boolean, onChange: (Boolean) -> Unit) {
    val u = LocalDims.current
    Row(title, detail, onClick = { onChange(!checked) }) {
        Switch(
            checked = checked, onCheckedChange = onChange, modifier = Modifier.scale(u.k * 1.15f),
            colors = SwitchDefaults.colors(checkedTrackColor = Accent, checkedThumbColor = Color.White,
                uncheckedTrackColor = Color.White.copy(alpha = 0.16f), uncheckedThumbColor = Color.White, uncheckedBorderColor = Color.Transparent),
        )
    }
}

/** A choice of a few, side by side. */
@Composable
private fun Segmented(options: List<String>, selected: Int, onPick: (Int) -> Unit) {
    val u = LocalDims.current
    Row(Modifier.fillMaxWidth().clip(RoundedCornerShape(u.d(22))).background(Color.White.copy(alpha = 0.08f)).padding(u.d(4)),
        horizontalArrangement = Arrangement.spacedBy(u.d(4))) {
        options.forEachIndexed { i, label ->
            val on = i == selected
            Box(
                Modifier.weight(1f).height(u.d(48)).clip(RoundedCornerShape(u.d(18)))
                    .background(if (on) Accent else Color.Transparent).clickable { onPick(i) },
                contentAlignment = Alignment.Center,
            ) {
                Text(label, color = if (on) Color(0xFF1A1330) else Ink, fontSize = u.t(19), fontWeight = FontWeight.Medium)
            }
        }
    }
}

@Composable
private fun ChoiceBlock(title: String, detail: String? = null, content: @Composable () -> Unit) {
    val u = LocalDims.current
    Column(Modifier.fillMaxWidth().padding(horizontal = u.d(24), vertical = u.d(16)), verticalArrangement = Arrangement.spacedBy(u.d(10))) {
        Text(title, color = Ink, fontSize = u.t(22), fontWeight = FontWeight.Medium)
        if (detail != null) Text(detail, color = InkSoft, fontSize = u.t(17))
        content()
    }
}

@Composable
private fun SmallButton(text: String, fill: Color = Color.White.copy(alpha = 0.10f), ink: Color = Ink, enabled: Boolean = true, onClick: () -> Unit) {
    val u = LocalDims.current
    Button(onClick = onClick, enabled = enabled, shape = RoundedCornerShape(50),
        colors = ButtonDefaults.buttonColors(containerColor = fill, contentColor = ink),
        modifier = Modifier.height(u.d(52))) {
        Text(text, fontSize = u.t(18), fontWeight = FontWeight.Medium)
    }
}

/** A text setting with its own Save button, so half-typed values are never used. */
@Composable
private fun Field(label: String, value: String, secret: Boolean = false, placeholder: String = "", onSave: (String) -> Unit) {
    val u = LocalDims.current
    var text by remember(value) { mutableStateOf(value) }
    Row(Modifier.fillMaxWidth().padding(horizontal = u.d(24), vertical = u.d(10)), verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(u.d(12))) {
        OutlinedTextField(
            value = text, onValueChange = { text = it }, singleLine = true,
            label = { Text(label, fontSize = u.t(16)) },
            placeholder = { if (placeholder.isNotEmpty()) Text(placeholder) },
            textStyle = androidx.compose.ui.text.TextStyle(fontSize = u.t(19), color = Ink),
            visualTransformation = if (secret) PasswordVisualTransformation() else VisualTransformation.None,
            keyboardOptions = KeyboardOptions(keyboardType = if (secret) KeyboardType.Password else KeyboardType.Text),
            modifier = Modifier.weight(1f),
        )
        SmallButton("Save", enabled = text != value) { onSave(text) }
    }
}

@Composable
private fun LinkCard(title: String, detail: String?, onClick: () -> Unit, modifier: Modifier = Modifier) {
    val u = LocalDims.current
    Row(
        modifier.heightIn(min = u.d(72)).clip(RoundedCornerShape(u.d(20))).background(Glass).clickable(onClick = onClick)
            .padding(horizontal = u.d(24)),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(title, color = Ink, fontSize = u.t(21), fontWeight = FontWeight.Medium)
        Spacer(Modifier.weight(1f))
        if (detail != null) Text(detail, color = InkSoft, fontSize = u.t(17), modifier = Modifier.padding(end = u.d(8)))
        Icon(Icons.Filled.KeyboardArrowRight, contentDescription = null, tint = InkSoft, modifier = Modifier.size(u.d(28)))
    }
}

// ---- The first page ----

@Composable
private fun MainPage(state: UiState, settings: Settings, two: Boolean, changed: (Boolean) -> Unit, onKeepScreenOn: () -> Unit,
                     openAdvanced: () -> Unit, openAbout: () -> Unit) {
    val u = LocalDims.current
    if (state.status.pairing) {
        PairingPanel(state.status)
        Spacer(Modifier.height(u.d(20)))
    } else if (!state.status.paired && !state.status.online) {
        LinkCard("Set up Muse", "Pair this Portal with the Muse app", openAdvanced, Modifier.fillMaxWidth())
        Spacer(Modifier.height(u.d(20)))
    }
    Columns(two,
        left = {
            SectionTitle("Talking")
            val voice = when {
                settings.museVoice -> 0
                settings.voice == Settings.BUILT_IN || settings.speechKey.isEmpty() -> 1
                else -> 2
            }
            Card(listOf(
                { ToggleRow("Video calls", "See Muse talk, as in the app", settings.videoCall) { settings.videoCall = it; changed(false) } },
                { ToggleRow("Live conversation", "Talk back and forth", settings.conversation) { settings.conversation = it; changed(false) } },
                {
                    ChoiceBlock("Voice", "When Muse answers one question at a time") {
                        Segmented(listOf("Muse", "Portal", "Cloud…"), voice) { i ->
                            when (i) {
                                0 -> settings.museVoice = true
                                1 -> { settings.museVoice = false; settings.voice = Settings.BUILT_IN }
                                else -> {
                                    settings.museVoice = false
                                    if (settings.speechKey.isEmpty()) openAdvanced()
                                    else if (settings.voice == Settings.BUILT_IN) settings.voice = Heymuse.cloudVoices().split(",").first()
                                }
                            }
                            changed(false)
                        }
                    }
                },
            ))
        },
        right = {
            SectionTitle("Listening")
            val levels = listOf(0.3f, 0.5f, 0.7f)
            val level = levels.indices.minBy { kotlin.math.abs(levels[it] - settings.sensitivity) }
            Card(listOf(
                { ToggleRow("“Hey Muse”", "Wake word, heard on this Portal", settings.wakeWord) { settings.wakeWord = it; changed(false) } },
                {
                    ChoiceBlock("How easily it wakes") {
                        Segmented(listOf("Less", "Normal", "More"), level) { i -> settings.sensitivity = levels[i]; changed(false) }
                    }
                },
            ))
            SectionTitle("Screen", top = true)
            Card(listOf(
                {
                    ToggleRow("Show my Muse", null, settings.myMuse) {
                        settings.myMuse = it; changed(false)
                        if (it) Assistant.syncAvatar()
                    }
                },
                { ToggleRow("Captions", "Write what’s said as well", settings.captions) { settings.captions = it; changed(false) } },
                { ToggleRow("Keep the screen on", null, settings.keepScreenOn) { settings.keepScreenOn = it; changed(false); onKeepScreenOn() } },
            ))
        },
    )
    TimersAndAlarms(state)
    Spacer(Modifier.height(u.d(20)))
    Row(horizontalArrangement = Arrangement.spacedBy(u.d(16))) {
        LinkCard("Advanced", "Pairing, token, voices", openAdvanced, Modifier.weight(2f))
        LinkCard("About", null, openAbout, Modifier.weight(1f))
    }
}

@Composable
private fun TimersAndAlarms(state: UiState) {
    val u = LocalDims.current
    if (state.timers.isEmpty() && state.alarms.isEmpty()) return
    Spacer(Modifier.height(u.d(16)))
    SectionTitle("Timers and alarms")
    Spacer(Modifier.height(u.d(10)))
    Card(
        state.timers.map { t ->
            @Composable {
                Row("${t.label.ifEmpty { "Timer" }} · ${countdown(t.endMs - System.currentTimeMillis())}") {
                    SmallButton("Cancel") { Assistant.cancelTimer(t.id) }
                }
            }
        } + state.alarms.map { a ->
            @Composable {
                Row("%02d:%02d".format(a.hour, a.minute), listOf(a.days, a.label, if (a.on) "" else "off").filter { it.isNotEmpty() }.joinToString(" · ")) {
                    SmallButton("Delete") { Assistant.deleteAlarm(a.id) }
                }
            }
        }
    )
}

// ---- Advanced ----

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun AdvancedPage(state: UiState, settings: Settings, two: Boolean, canOverlay: Boolean, onAllowOverlay: () -> Unit,
                         onKeepScreenOn: () -> Unit, changed: (Boolean) -> Unit, phoneSetup: () -> Unit) {
    val u = LocalDims.current
    val s = state.status
    if (s.pairing) {
        PairingPanel(s)
        Spacer(Modifier.height(u.d(20)))
    }
    Columns(two,
        left = {
            SectionTitle("Muse")
            val token = settings.sdkToken
            Card(listOfNotNull(
                {
                    Row(
                        when {
                            s.online -> "Paired as “${s.agentName.ifEmpty { "Muse" }}”"
                            s.paired -> "Paired, ${if (s.error.isNotEmpty()) "not connected" else "connecting"}"
                            else -> "Not paired yet"
                        },
                        if (s.paired && s.error.isNotEmpty()) s.error else s.bleName.ifEmpty { null },
                    ) {
                        if (!s.pairing) SmallButton(if (s.paired) "Pair again" else "Pair", enabled = token.isNotEmpty()) { Assistant.startPairing() }
                    }
                },
                {
                    Row("SDK token", if (token.isEmpty()) "Not set · from gadgets.muse.ai" else "Saved") {
                        SmallButton("Set from phone", onClick = phoneSetup)
                    }
                },
                { Field("Or type the token", token, secret = true, placeholder = "mgst_…") { settings.sdkToken = it; changed(false) } },
                { Field("Name in the Muse app", settings.displayName) { settings.displayName = it; changed(false) } },
                if (s.paired) {
                    {
                        Row("Forget this Portal", "Remove it in the Muse app too") {
                            SmallButton("Unpair", fill = Hangup.copy(alpha = 0.16f), ink = Color(0xFFFFB59E)) { Assistant.unpair() }
                        }
                    }
                } else null,
            ))
            if (s.pairingFailed && !s.paired) Text("The last pairing failed: ${s.pairingReason.ifEmpty { s.pairingError }}", color = Warning, fontSize = u.t(17))
            if (state.problem.isNotEmpty()) Text(state.problem, color = Warning, fontSize = u.t(17))

            SectionTitle("Cloud voice", top = true)
            Card(listOf(
                {
                    ChoiceBlock("OpenAI-style speech", "Used only when Muse’s own voice isn’t available. The answer’s words go to that service.") {
                        val voices = Heymuse.cloudVoices().split(",").filter { it.isNotBlank() }
                        FlowRow(horizontalArrangement = Arrangement.spacedBy(u.d(8)), verticalArrangement = Arrangement.spacedBy(u.d(8))) {
                            voices.forEach { v ->
                                val on = settings.voice == v
                                Box(
                                    Modifier.height(u.d(44)).clip(RoundedCornerShape(50))
                                        .background(if (on) Accent else Color.White.copy(alpha = 0.08f))
                                        .clickable(enabled = settings.speechKey.isNotEmpty()) { settings.voice = v; changed(false) }
                                        .padding(horizontal = u.d(16)),
                                    contentAlignment = Alignment.Center,
                                ) { Text(v, color = if (on) Color(0xFF1A1330) else Ink, fontSize = u.t(17)) }
                            }
                        }
                    }
                },
                { Field("Speech key", settings.speechKey, secret = true, placeholder = "sk-…") { settings.speechKey = it; changed(false) } },
                { Field("How to say it", settings.speechStyle, placeholder = "warmly, and not too fast") { settings.speechStyle = it; changed(false) } },
                { Field("Endpoint", settings.speechBase, placeholder = "https://api.openai.com/v1") { settings.speechBase = it; changed(false) } },
                { Field("Model", settings.speechModel, placeholder = "gpt-4o-mini-tts") { settings.speechModel = it; changed(false) } },
                { Row("Hear the voice") { SmallButton("Play") { Assistant.testVoice() } } },
            ))
        },
        right = {
            SectionTitle("This Portal")
            Card(listOfNotNull(
                { ToggleRow("Start with the Portal", null, settings.startOnBoot) { settings.startOnBoot = it; changed(false) } },
                { ToggleRow("Come forward on “Hey Muse”", "From behind other apps", settings.openOnWake) { settings.openOnWake = it; changed(false) } },
                if (settings.openOnWake && !canOverlay) {
                    { Row("Needs “Display over other apps”") { SmallButton("Allow", onClick = onAllowOverlay) } }
                } else null,
                { ToggleRow("Portal voice processing", "Echo and noise cancelling", settings.voiceProcessing) { settings.voiceProcessing = it; changed(true) } },
                { ToggleRow("Muse’s own voice", "For one-question answers", settings.museVoice) { settings.museVoice = it; changed(false) } },
            ))
            Column(Modifier.fillMaxWidth().padding(top = u.d(10)).clip(RoundedCornerShape(u.d(24))).background(Glass).padding(u.d(24)),
                verticalArrangement = Arrangement.spacedBy(u.d(6))) {
                Text("Tip", color = Ink, fontSize = u.t(21), fontWeight = FontWeight.Medium)
                Text("Turn off “Hey Portal” and Alexa in the Portal’s own settings, so the microphone is Hey Muse’s alone.",
                    color = InkSoft, fontSize = u.t(17), lineHeight = u.t(24))
            }
        },
    )
}

@Composable
private fun PairingPanel(s: MuseStatus) {
    val u = LocalDims.current
    Column(
        Modifier.fillMaxWidth().clip(RoundedCornerShape(u.d(24))).background(Accent.copy(alpha = 0.12f)).padding(u.d(28)),
        verticalArrangement = Arrangement.spacedBy(u.d(10)),
    ) {
        Text("This Portal is on Bluetooth as", color = InkSoft, fontSize = u.t(19))
        Text(s.bleName, color = Ink, fontSize = u.t(40), fontFamily = FontFamily.Monospace, fontWeight = FontWeight.SemiBold)
        Text("In the Muse app: Settings → Devices, turn on Developer mode, tap + and pick ${s.bleName}. " +
            "Continue past the community-device warning, and pick the one Wi-Fi network shown.",
            color = Ink, fontSize = u.t(19), lineHeight = u.t(27))
        Text("Now: ${s.pairingState}${if (s.pairingReason.isNotEmpty()) " (${s.pairingReason})" else ""} · " +
            "open for ${Heymuse.PairingWindowSeconds / 60} minutes", color = Accent, fontSize = u.t(18))
        Row(horizontalArrangement = Arrangement.spacedBy(u.d(12))) {
            SmallButton("Stop pairing") { Assistant.cancelPairing() }
        }
    }
}

// ---- About ----

@Composable
private fun AboutPage(avatarLoaded: Boolean, avatarPath: String) {
    val u = LocalDims.current
    Column(Modifier.fillMaxWidth().clip(RoundedCornerShape(u.d(24))).background(Glass).padding(u.d(28)),
        verticalArrangement = Arrangement.spacedBy(u.d(14))) {
        Text("Hey Muse ${BuildConfig.VERSION_NAME}", color = Ink, fontSize = u.t(26), fontWeight = FontWeight.SemiBold)
        Text("A hobby project, not made or endorsed by Meta. Muse is Meta’s, and using it this way is covered by Meta’s Gadget SDK Terms.",
            color = InkSoft, fontSize = u.t(19), lineHeight = u.t(27))
        Text("After “Hey Muse”, what you say goes to your Muse and is kept in your Muse account. Anyone who speaks to the Portal is " +
            "recorded into your account the same way. In a call, Muse hears the room until the call ends.",
            color = InkSoft, fontSize = u.t(19), lineHeight = u.t(27))
        Text(if (avatarLoaded) "A Muse character from your own copy of Meta’s SDK is installed."
            else "Optional: a character made from your own copy of Meta’s SDK (tools/muse-avatar), pushed to $avatarPath.",
            color = InkMuted, fontSize = u.t(16), lineHeight = u.t(23))
    }
}
