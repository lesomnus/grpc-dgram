import { defineConfig } from 'tsdown'

export default defineConfig({
  entry: {
    index: 'src/index.ts',
    webrtc: 'src/webrtc.ts',
    protobufes: 'src/protobufes.ts',
  },
  dts: true,
})
