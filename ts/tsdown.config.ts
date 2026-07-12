import { defineConfig } from 'tsdown'

export default defineConfig({
  entry: {
    index: 'src/index.ts',
    webrtc: 'src/webrtc.ts',
  },
  dts: true,
})
