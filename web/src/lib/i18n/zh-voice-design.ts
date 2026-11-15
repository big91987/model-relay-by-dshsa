// Chinese translations for the voice-design console surface.
export const zh: Record<string, string> = {
  Synthesizing: '合成中',
  Ready: '可播放',
  'Your browser cannot play this audio.': '你的浏览器无法播放这段音频。',
  'The preview audio is not available.': '预览音频不可用。',
  Selected: '已选中',
  'Use this voice': '使用此音色',
  'Failed to load voice design': '加载音色设计失败',
  'Failed to refresh voice design': '刷新音色设计失败',
  'Add a MiniMax channel with a speech model first': '请先添加带语音模型的 MiniMax 渠道',
  'Describe the voice you want to design': '请描述你想设计的音色',
  'Enter the preview text for the voice sample': '请输入音色示例的预览文本',
  'Voice designed': '音色设计完成',
  'Failed to design the voice': '设计音色失败',
  'Enter the text this voice should read': '请输入该音色要朗读的文本',
  'Long text started as a background task': '长文本已作为后台任务开始处理',
  'Speech generated': '语音已生成',
  'Failed to generate speech': '生成语音失败',
  'Describe a voice, preview the sample MiniMax generates, then use that voice to read any text.':
    '描述想要的声音，试听 MiniMax 生成的示例，然后用该音色朗读任意文本。',
  'Loading voice design…': '正在加载音色设计…',
  'No speech model': '暂无语音模型',
  'Add a MiniMax channel with a speech model such as speech-2.8-hd, then come back here to design a voice from a description.':
    '请添加带语音模型（如 speech-2.8-hd）的 MiniMax 渠道，然后返回此处根据描述设计音色。',
  'Add a channel': '添加渠道',
  'Design a voice': '设计音色',
  'Describe the voice you want. MiniMax creates a custom voice and reads the preview text as a sample.':
    '描述你想要的声音。MiniMax 会创建自定义音色，并朗读预览文本作为示例。',
  'Voice description': '音色描述',
  '{n} / {max} characters': '{n} / {max} 字符',
  'A warm, calm female narrator in her thirties with a gentle, unhurried delivery and a hint of curiosity.':
    '一位三十多岁的女声旁白，温暖而平静，语速舒缓，带有一丝好奇。',
  'Preview text': '试听文本',
  Download: '下载',
  Preview: '预览',
  'The sample sentence the designed voice reads in its preview.': '设计出的音色在预览中朗读的示例句子。',
  'Designing generates a paid MiniMax preview sample ($30 per 1M characters). The preview text is capped at {max} characters.':
    '设计会生成一段付费的 MiniMax 预览示例（每 100 万字符 $30）。预览文本上限为 {max} 字符。',
  'Designing voice…': '正在设计音色…',
  'Design voice': '设计音色',
  'The designed voice and its preview are stored on the gateway. Pick it in the list to replay the sample.':
    '设计好的音色及其预览保存在网关上。在列表中选择即可重放示例。',
  'Use a designed voice': '使用设计好的音色',
  'Preview the sample, then read another piece of text with the selected voice.':
    '先试听示例，再用所选音色朗读另一段文本。',
  'Design a voice, or select one from the list, to preview it and read new text.':
    '设计音色，或从列表中选择一个，即可试听并朗读新文本。',
  'No designed voices': '暂无设计好的音色',
  'Describe a voice on the left, generate its preview, and it will appear here so you can read new text with it.':
    '在左侧描述声音并生成预览，音色会出现在此处，之后可用它朗读新文本。',
  'Read another text': '朗读其他文本',
  '{n} / {max} characters · runs in the background': '{n} / {max} 字符 · 将在后台运行',
  'Type the text this designed voice should read aloud…': '输入希望该音色朗读的文本…',
  Speed: '语速',
  'Text over {n} characters is split into parts and synthesized in the background on the channel that owns the voice.':
    '超过 {n} 字符的文本会被拆分，并在拥有该音色的渠道上于后台合成。',
  'Starting…': '正在开始…',
  'Start background task': '开始后台任务',
  'Read with this voice': '使用该音色朗读',
  'Generated speech': '已生成语音',
  '{n} characters': '{n} 字符',
  'Download {format}': '下载 {format}',
  'Text over {n} characters is submitted as one asynchronous MiniMax task on the channel that owns the voice.':
    '超过 {n} 字符的文本将作为一个异步 MiniMax 任务提交到拥有该音色的渠道。',
  'Long text submitted as one background task': '长文本已作为一个后台任务提交',
  'Submitting the task…': '正在提交任务…',
  'MiniMax is generating the full audio as one task…': 'MiniMax 正在作为一个任务生成完整音频…',
  'This is one MiniMax task; refreshing the page or restarting the gateway keeps following it.':
    '这是一个 MiniMax 任务；刷新页面或重启网关后会继续跟踪它。',
  'Waiting for MiniMax to finish the full audio': '正在等待 MiniMax 完成完整音频',

  'Open speech history': '打开语音历史',
  'Speech synthesis failed.': '语音合成失败。',
  'Waiting to start…': '等待开始…',
  'Synthesizing part {index} of {total}…': '正在合成第 {index} 部分，共 {total} 部分…',
  'Speech synthesis progress': '语音合成进度',
  'This task keeps running in the background; the finished audio appears here automatically.':
    '该任务会在后台继续运行，完成的音频会自动显示在此处。',
  'Designed voices': '已设计的音色',
  '{n} in this workspace': '此工作区共 {n} 个',
}
