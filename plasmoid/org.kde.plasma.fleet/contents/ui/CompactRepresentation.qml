import QtQuick
import QtQuick.Layouts
import org.kde.plasma.plasmoid
import org.kde.plasma.core as PlasmaCore
import org.kde.kirigami as Kirigami
import org.kde.plasma.components as PlasmaComponents3

Item {
    id: compactRoot
    Layout.fillWidth: true
    Layout.fillHeight: true

    Kirigami.Icon {
        anchors.centerIn: parent
        width: Math.min(parent.width, parent.height) - Kirigami.Units.smallSpacing
        height: width
        source: root.Plasmoid.icon
    }

    // Active working indicator dot (blue)
    Rectangle {
        visible: (root.fleetStatus.working_count || 0) > 0
        width: Kirigami.Units.gridUnit * 0.4
        height: width
        radius: width / 2
        color: Kirigami.Theme.highlightColor
        anchors.bottom: parent.bottom
        anchors.right: parent.right
        anchors.margins: 1
    }

    // Review pending indicator dot (green/yellow)
    Rectangle {
        visible: (root.fleetStatus.review_count || 0) > 0
        width: Kirigami.Units.gridUnit * 0.4
        height: width
        radius: width / 2
        color: Kirigami.Theme.positiveTextColor
        anchors.top: parent.top
        anchors.right: parent.right
        anchors.margins: 1
    }

    MouseArea {
        anchors.fill: parent
        onClicked: root.expanded = !root.expanded
    }
}
