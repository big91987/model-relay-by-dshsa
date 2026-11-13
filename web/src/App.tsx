import { Navigate, Route, Routes } from 'react-router-dom'
import { AppLayout } from './components/AppLayout'
import { Spinner } from './components/ui'
import { useAuth } from './lib/auth'
import Channels from './pages/Channels'
import Dashboard from './pages/Dashboard'
import Login from './pages/Login'
import Logs from './pages/Logs'
import Models from './pages/Models'
import Playground from './pages/Playground'
import Speech from './pages/Speech'
import Tokens from './pages/Tokens'
import Videos from './pages/Videos'
import VoiceDesignPage from './pages/VoiceDesign'

function BootScreen() {
  return (
    <div className="boot-screen">
      <div className="brand-mark large">MR</div>
      <Spinner size={20} />
    </div>
  )
}

function Protected() {
  const { user, loading } = useAuth()
  if (loading) return <BootScreen />
  if (!user) return <Navigate to="/login" replace />
  return <AppLayout />
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route element={<Protected />}>
        <Route path="/dashboard" element={<Dashboard />} />
        <Route path="/playground" element={<Playground />} />
        <Route path="/channels" element={<Channels />} />
        <Route path="/models" element={<Models />} />
        <Route path="/videos" element={<Videos />} />
        <Route path="/speech" element={<Speech />} />
        <Route path="/voice-design" element={<VoiceDesignPage />} />
        <Route path="/tokens" element={<Tokens />} />
        <Route path="/logs" element={<Logs />} />
      </Route>
      <Route path="/" element={<Navigate to="/dashboard" replace />} />
      <Route path="*" element={<Navigate to="/dashboard" replace />} />
    </Routes>
  )
}
